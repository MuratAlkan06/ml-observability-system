#!/usr/bin/env bash
#
# The operator's end-to-end check on k3d — Phase 3 O2 (docs/PLAN.md D35).
#
#   IMAGE_TAG=<sha> CANARY_TAG=<sha> [IMAGE_PREFIX=docker.io/library] [HELM=helm] \
#     operator/hack/e2e.sh
#
# Runs against the current kubectl context: a k3d cluster on the pinned k3s
# image, started WITHOUT the D34 flag pair, into which these have been
# imported (CI's OperatorE2E job builds and imports them; operator/README.md
# has the same steps for a laptop). It needs helm on PATH, or in HELM; CI
# installs the pinned v3.22.0 (D38).
#   mlobs-operator:IMAGE_TAG   the operator, built from operator/Dockerfile
#   mlobs-stub:e2e             operator/hack/e2e-stub.Dockerfile
#   mlobs-api:IMAGE_TAG        the stub again, as the stable api
#   mlobs-api:CANARY_TAG       the stub again, as the canary
# The stub answers GET /health on 8000 and nothing else, so this is the
# operator's mechanics — leader election and the reconcile — and not the
# stack's: K3sSmoke rehearses the stack, with the real images, in both states.
#
# What it asserts, in order:
#   1. apply.sh's preflight refuses the cluster's default node-port range on
#      its fixed line, and nothing was applied
# then the same four, once for each way the operator is installed — first
# from the flattened manifests as apply.sh renders them, then, on the same
# cluster emptied of the first install down to its CRD, from the Helm chart
# at deploy/helm/mlobs-operator with `helm install -n mlobs` (Phase 3 H1,
# D38):
#   2. the operator holds its Lease (leader election, asserted as the
#      acquisition only — D35)
#   3. its ServiceAccount may patch the shadow scorer's scale and nothing
#      beside it (the D33 addendum of O2)
#   4. the ServingDeployment's deployment/api is created, controlled by it,
#      at the rendered image, and Ready at the current generation
#   5. a canary window opens in D37's order — the shadow scorer paused with
#      no pods before the canary exists — and apply.sh's own close-window
#      patch closes it in the reverse order
# and, for the chart install only:
#   6. the release is deployed and the operator it runs is the release's, and
#      `helm uninstall` leaves the CRD in place: crds/ is install-only
set -euo pipefail
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
MANIFESTS="${REPO_ROOT}/deploy/k3s/manifests"
CHART="${REPO_ROOT}/deploy/helm/mlobs-operator"

IMAGE_PREFIX="${IMAGE_PREFIX:-docker.io/library}"
IMAGE_TAG="${IMAGE_TAG:-}"
CANARY_TAG="${CANARY_TAG:-}"
STUB_IMAGE="${STUB_IMAGE:-${IMAGE_PREFIX}/mlobs-stub:e2e}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-180}"
HELM="${HELM:-helm}"

NAMESPACE=mlobs
CRD_NAME=servingdeployments.serving.mlobs.dev
SERVINGDEPLOYMENT=servingdeployment/api
OPERATOR_IDENTITY="system:serviceaccount:${NAMESPACE}:operator"
RELEASE=mlobs-operator

die() {
  echo "error: $1" >&2
  exit 1
}

[[ "$IMAGE_TAG" =~ ^[0-9a-f]{40}$ ]] || die "IMAGE_TAG must be a 40-character lowercase hex tag"
[[ "$CANARY_TAG" =~ ^[0-9a-f]{40}$ ]] || die "CANARY_TAG must be a 40-character lowercase hex tag"
[ "$CANARY_TAG" != "$IMAGE_TAG" ] || die "CANARY_TAG must differ from IMAGE_TAG"
command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
command -v "$HELM" >/dev/null 2>&1 || die "helm not found (HELM=${HELM})"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

k() {
  kubectl --namespace "$NAMESPACE" "$@"
}

# The manifests as apply.sh renders them.
render() {
  sed -e "s|IMAGE_PREFIX|${IMAGE_PREFIX}|g" -e "s|IMAGE_TAG|${IMAGE_TAG}|g" "${MANIFESTS}/$1"
}

# poll_until WHAT COMMAND...: run COMMAND until it succeeds, to a deadline.
# Bounded, the only sleep this repo sanctions (PRINCIPLES.md §5).
poll_until() {
  local what="$1" deadline=$((SECONDS + TIMEOUT_SECONDS))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die "${what}: not reached within ${TIMEOUT_SECONDS}s"
    sleep 2
  done
}

# field RESOURCE JSONPATH: one field of a namespaced object.
field() {
  k get "$1" -o "jsonpath=$2" 2>/dev/null
}

# no_live_pods SELECTOR: no pod matching SELECTOR is starting or running. A
# pod told to stop whose containers have all exited is moved to a terminal
# phase, Succeeded or Failed, by the kubelet, and its object can linger a
# moment for cleanup; it holds no memory, which is what D37's order is for,
# and the operator's own "gone" does not count it either.
no_live_pods() {
  local phases
  phases="$(k get pods -l "$1" -o 'jsonpath={range .items[*]}{.status.phase}{"\n"}{end}')" || return 1
  ! printf '%s\n' "$phases" | grep -qE '^(Pending|Running|Unknown)$'
}

# --- 1. the preflight refuses a default node-port range ----------------------

printf 'POSTGRES_PASSWORD=e2e-postgres\nGF_ADMIN_PASSWORD=e2e-grafana\n' > "${work_dir}/e2e.env"
status=0
IMAGE_PREFIX="$IMAGE_PREFIX" IMAGE_TAG="$IMAGE_TAG" ENV_FILE="${work_dir}/e2e.env" \
  "${REPO_ROOT}/deploy/k3s/apply.sh" >"${work_dir}/apply.out" 2>"${work_dir}/apply.err" || status=$?
[ "$status" -ne 0 ] || die "apply.sh passed its preflight on a cluster without the D34 range"
grep -qF "error: preflight: the cluster's node-port range does not admit 8000" "${work_dir}/apply.err" \
  || die "apply.sh failed, but not on the preflight's fixed line"
if grep -q '^ok:' "${work_dir}/apply.out"; then
  die "apply.sh reported a step done before its preflight refused the cluster"
fi
if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  die "namespace ${NAMESPACE} exists after the preflight refused the cluster"
fi
if kubectl get crd "$CRD_NAME" >/dev/null 2>&1; then
  die "crd/${CRD_NAME} exists after the preflight refused the cluster"
fi
echo "ok: apply.sh's preflight refused the default node-port range on its fixed line; nothing was applied"


# --- the operator's two installs ---------------------------------------------

# install_flattened: the namespace, the CRD until Established, then the RBAC
# and the operator, from the flattened manifests as apply.sh renders them.
install_flattened() {
  render 00-namespace.yaml | kubectl apply -f - >/dev/null
  render 01-servingdeployment-crd.yaml | kubectl apply -f - >/dev/null
  kubectl wait --for=condition=Established "crd/${CRD_NAME}" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  render 02-operator-rbac.yaml | kubectl apply -f - >/dev/null
  render 03-operator.yaml | kubectl apply -f - >/dev/null
}

# install_chart: the namespace from the flattened manifests, as the EKS
# demonstration applies it (D39), then the chart into mlobs at the same
# prefix and tag. The CRD comes from the chart's crds/, which Helm installs
# only when it is absent: it is, after uninstall_flattened.
install_chart() {
  render 00-namespace.yaml | kubectl apply -f - >/dev/null
  "$HELM" install "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
    --set image.prefix="$IMAGE_PREFIX" --set image.tag="$IMAGE_TAG" >/dev/null
  kubectl wait --for=condition=Established "crd/${CRD_NAME}" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
}

# uninstall_flattened: the first install taken back to the cluster step 1
# left — the ServingDeployment first, while the operator still runs, as D36
# deletes it; then the namespace and everything in it; then the CRD.
uninstall_flattened() {
  k delete "$SERVINGDEPLOYMENT" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  kubectl delete namespace "$NAMESPACE" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  kubectl delete crd "$CRD_NAME" --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
    die "namespace ${NAMESPACE} still exists after its delete"
  fi
  if kubectl get crd "$CRD_NAME" >/dev/null 2>&1; then
    die "crd/${CRD_NAME} still exists after its delete"
  fi
  echo "ok: the flattened install removed: ${SERVINGDEPLOYMENT}, namespace ${NAMESPACE} and crd/${CRD_NAME} gone"
}

# --- 2.-5. what each install must do ------------------------------------------

lease="$(sed -n 's/^[[:space:]]*leaderElectionID = "\(.*\)"$/\1/p' "${REPO_ROOT}/operator/cmd/main.go")"
[ -n "$lease" ] || die "leaderElectionID not found in operator/cmd/main.go"
operator_pod=""
# A holder identity is the holder's hostname, its pod name, then _ and a UUID.
lease_held() {
  local holder
  holder="$(field "lease/${lease}" '{.spec.holderIdentity}')"
  [ "${holder%%_*}" = "$operator_pod" ]
}

# can_i EXPECTED VERB RESOURCE [FLAGS...]: kubectl auth can-i as the
# operator's ServiceAccount prints yes or no, and exits non-zero on no.
can_i() {
  local expected="$1" answer
  shift
  answer="$(kubectl auth can-i "$@" --namespace "$NAMESPACE" --as "$OPERATOR_IDENTITY" 2>/dev/null)" || true
  [ "$answer" = "$expected" ] || die "can-i $* as the operator: expected ${expected}, got ${answer:-no answer}"
  echo "ok: can-i $* as the operator: ${answer}"
}

ready_at_generation() {
  local generation observed ready ready_generation
  read -r generation observed ready ready_generation <<<"$(field "$SERVINGDEPLOYMENT" \
    '{.metadata.generation} {.status.observedGeneration} {.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].observedGeneration}')"
  [ -n "$generation" ] && [ "$generation" = "$observed" ] \
    && [ "$ready" = True ] && [ "$ready_generation" = "$generation" ]
}

shadow_back() {
  [ "$(field statefulset/shadow-scorer '{.spec.replicas}')" = 1 ]
}

# The close is apply.sh's constant, read from apply.sh, not a copy of it.
close_patch="$(sed -n "s/^CLOSE_WINDOW_PATCH='\(.*\)'\$/\1/p" "${REPO_ROOT}/deploy/k3s/apply.sh")"
[ -n "$close_patch" ] || die "CLOSE_WINDOW_PATCH not found in deploy/k3s/apply.sh"

# assert_operator INSTALL: steps 2 to 5 against the operator INSTALL put in
# place, the same for both.
assert_operator() {
  echo "ok: --- the operator installed from $1 ---"

  # --- 2. the operator holds its Lease ---------------------------------------

  # redis because the api's pod template waits on it in an init container;
  # the shadow scorer's place is taken by the stub, under the name and labels
  # the operator pauses.
  render 11-redis.yaml | kubectl apply -f - >/dev/null
  kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: shadow-scorer
  namespace: ${NAMESPACE}
  labels:
    app: shadow-scorer
spec:
  serviceName: shadow-scorer
  selector:
    matchLabels:
      app: shadow-scorer
  template:
    metadata:
      labels:
        app: shadow-scorer
    spec:
      containers:
        - name: shadow-scorer
          image: ${STUB_IMAGE}
          imagePullPolicy: IfNotPresent
EOF
  for workload in deployment/operator deployment/redis statefulset/shadow-scorer; do
    k rollout status "$workload" --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
      || die "rollout did not converge within ${TIMEOUT_SECONDS}s: ${workload}"
  done
  echo "ok: operator, redis and the stub shadow scorer rolled out"

  operator_pod="$(k get pods -l app=operator -o 'jsonpath={.items[0].metadata.name}')"
  [ -n "$operator_pod" ] || die "no operator pod found"
  poll_until "lease/${lease} held by pod/${operator_pod}" lease_held
  echo "ok: lease/${lease} acquired by pod/${operator_pod}"

  # --- 3. the operator's grant, as the API server sees it --------------------

  can_i yes patch statefulsets.apps/shadow-scorer --subresource=scale
  can_i no patch statefulsets.apps/consumer --subresource=scale
  can_i no patch statefulsets.apps/shadow-scorer
  can_i no delete statefulsets.apps
  can_i no get secrets

  # --- 4. deployment/api created, owned and Ready ----------------------------

  render 40-servingdeployment.yaml | kubectl apply -f - >/dev/null
  poll_until "${SERVINGDEPLOYMENT} Ready at its current generation" ready_at_generation

  local owner image canary_image
  owner="$(field deployment/api \
    '{.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}')"
  [ "$owner" = "ServingDeployment/api" ] || die "deployment/api is controlled by '${owner}', not ServingDeployment/api"
  image="$(field deployment/api '{.spec.template.spec.containers[?(@.name=="api")].image}')"
  [ "$image" = "${IMAGE_PREFIX}/mlobs-api:${IMAGE_TAG}" ] || die "deployment/api runs ${image}, not the rendered tag"
  echo "ok: deployment/api created, controlled by servingdeployment/api at ${image}; Ready at its generation"

  # --- 5. one canary window, opened and closed in order ----------------------

  k patch "$SERVINGDEPLOYMENT" --type merge \
    -p "{\"spec\":{\"canaryImageTag\":\"${CANARY_TAG}\",\"canaryReplicas\":1}}" >/dev/null
  k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  k wait "$SERVINGDEPLOYMENT" --for=condition=ShadowPaused --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  [ "$(field statefulset/shadow-scorer '{.spec.replicas}')" = 0 ] || die "the shadow scorer is not at 0 in an open window"
  poll_until "deployment/api-canary created" k get deployment/api-canary
  # The canary exists only once the shadow scorer's pods are gone (D37).
  no_live_pods app=shadow-scorer || die "deployment/api-canary exists while a shadow scorer pod still runs"
  k rollout status deployment/api-canary --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
    || die "rollout did not converge within ${TIMEOUT_SECONDS}s: deployment/api-canary"
  canary_image="$(field deployment/api-canary '{.spec.template.spec.containers[?(@.name=="api")].image}')"
  [ "$canary_image" = "${IMAGE_PREFIX}/mlobs-api:${CANARY_TAG}" ] || die "deployment/api-canary runs ${canary_image}"
  echo "ok: window open: CanaryActive True, shadow scorer at 0 with no pods, then deployment/api-canary at ${canary_image}"

  k patch "$SERVINGDEPLOYMENT" --type merge -p "$close_patch" >/dev/null
  k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive=false --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  poll_until "statefulset/shadow-scorer back at 1" shadow_back
  # The shadow scorer returns only once the canary's pods are gone (D37).
  no_live_pods app=api,role=canary || die "the shadow scorer is back at 1 while a canary pod still runs"
  [ "$(field deployment/api-canary '{.spec.replicas}')" = 0 ] || die "deployment/api-canary is not at 0 after the close"
  k rollout status statefulset/shadow-scorer --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
    || die "rollout did not converge within ${TIMEOUT_SECONDS}s: statefulset/shadow-scorer"
  k wait "$SERVINGDEPLOYMENT" --for=condition=ShadowPaused=false --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  echo "ok: window closed by apply.sh's patch: CanaryActive False, canary at 0 with no pods, then the shadow scorer at 1"

  echo "ok: the operator's events, oldest first:"
  k get events --field-selector involvedObject.kind=ServingDeployment \
    --sort-by=.metadata.creationTimestamp -o 'custom-columns=REASON:.reason,MESSAGE:.message' --no-headers \
    | sed 's/^/    /'
}

# --- the flattened manifests, then the chart ---------------------------------

install_flattened
assert_operator "the flattened manifests"
uninstall_flattened

echo "ok: helm $("$HELM" version --template '{{.Version}}')"
install_chart
assert_operator "the chart (helm install -n ${NAMESPACE})"

# --- 6. the chart's release, and its install-only CRD ------------------------

deployed="$("$HELM" list --namespace "$NAMESPACE" --deployed --short)"
[ "$deployed" = "$RELEASE" ] || die "helm lists '${deployed}' deployed in ${NAMESPACE}, not ${RELEASE}"
managed="$(field deployment/operator \
  '{.metadata.labels.app\.kubernetes\.io/managed-by} {.metadata.annotations.meta\.helm\.sh/release-name}')"
[ "$managed" = "Helm ${RELEASE}" ] || die "deployment/operator is not the release's: '${managed}'"
echo "ok: release ${RELEASE} deployed in ${NAMESPACE}; deployment/operator is its (managed-by Helm, release ${RELEASE})"

"$HELM" uninstall "$RELEASE" --namespace "$NAMESPACE" --wait --timeout "${TIMEOUT_SECONDS}s" >/dev/null
left="$(k get deployment/operator role/operator rolebinding/operator serviceaccount/operator \
  --ignore-not-found -o name)"
[ -z "$left" ] || die "helm uninstall left the release's objects: ${left}"
kubectl get crd "$CRD_NAME" >/dev/null 2>&1 \
  || die "crd/${CRD_NAME} is gone after helm uninstall; crds/ should be install-only"
k get "$SERVINGDEPLOYMENT" >/dev/null 2>&1 \
  || die "${SERVINGDEPLOYMENT} is gone after helm uninstall; the CRD it lives under should have stayed"
echo "ok: helm uninstall removed the operator and its RBAC, and left crd/${CRD_NAME} and ${SERVINGDEPLOYMENT} in place: crds/ is install-only"

echo "ok: operator e2e passed, from the flattened manifests and from the chart"
