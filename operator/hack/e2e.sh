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
# The stub answers GET /health on 8000, and GET /metrics with the two /predict
# series the gate reads, whose counters this script shapes pod by pod through
# a file in the pod (e2e-stub.Dockerfile); it serves nothing else. So this is
# the operator's mechanics — leader election, the reconcile and the gate — and
# not the stack's: K3sSmoke rehearses the stack, with the real images, in both
# states.
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
#      beside it (the D33 addendum of O2), and may get and list pods but not
#      watch them nor reach them through pods/proxy (D41)
#   4. the ServingDeployment's deployment/api is created, controlled by it,
#      at the rendered image, and Ready at the current generation
#   5. a canary window opens in D37's order — the shadow scorer paused with
#      no pods before the canary exists — and apply.sh's own close-window
#      patch closes it in the reverse order; with no spec.gate the operator
#      is v0 — no status.gate, no gate Event — and the installed operator
#      does not set the gate's test-only interval knob
# for the flattened install only:
#   7. the gate in Observe (D42, D45), on one window whose grid the test-only
#      knob shortens to 20 s — the operator's own Deployment patched here and
#      nowhere else; no scenario runs at the real 60 s interval. Each verdict
#      sequence is read from status.gate: pending while the canary cannot
#      start, then warmup, then eleven clean intervals and the promote
#      verdict; a latency breach two in a row and the rollback verdict, the
#      window left open and the canary serving; a canary /metrics that never
#      answers, unevaluable at each boundary with the reconcile unblocked —
#      the operator's reads of pod /metrics at runtime, under the Role that
#      ships, with no watch (D41); then a pipeline deploy — the CR apply and
#      the close-window patch, as apply.sh sends them — leaves spec.gate as
#      it was, and the rendered CR carries no gate key
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
# The gate's waits are counted in intervals of its shortened grid: the clean
# run alone is warmup plus eleven of them.
GATE_TIMEOUT_SECONDS="${GATE_TIMEOUT_SECONDS:-900}"
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
  poll_within "$TIMEOUT_SECONDS" "$@"
}

# poll_within SECONDS WHAT COMMAND...: poll_until, to a deadline of its own.
poll_within() {
  local limit="$1" what="$2" deadline
  shift 2
  deadline=$((SECONDS + limit))
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die "${what}: not reached within ${limit}s"
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

# The gate's test-only interval knob, read from the operator's source, and the
# grid step 7 runs on: the knob's floor, four read timeouts (D41).
gate_knob="$(sed -n 's/^const gateIntervalEnv = "\(.*\)"$/\1/p' "${REPO_ROOT}/operator/internal/controller/gate.go")"
[ -n "$gate_knob" ] || die "gateIntervalEnv not found in operator/internal/controller/gate.go"
GATE_INTERVAL=20s

# gate_events: the reasons of the gate's Events on the ServingDeployment, one
# a line.
gate_events() {
  k get events --field-selector involvedObject.kind=ServingDeployment \
    -o 'jsonpath={range .items[*]}{.reason}{"\n"}{end}' | grep '^Gate' || true
}

# knob_unset: the operator Deployment as installed does not set the knob.
knob_unset() {
  local names
  names="$(field deployment/operator '{.spec.template.spec.containers[*].env[*].name}')" || return 1
  ! printf '%s\n' "$names" | tr ' ' '\n' | grep -qxF "$gate_knob"
}

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
  # Metric acquisition (D41): pods read at boundaries through the uncached
  # APIReader, so get and list and no watch; /metrics at the pod IP, so no
  # pods/proxy. The pods grant reads and nothing more: no pod written, no
  # subresource that reaches into one, and no ConfigMap read beside it.
  can_i yes get pods
  can_i yes list pods
  can_i no watch pods
  can_i no create pods
  can_i no delete pods
  can_i no get pods --subresource=proxy
  can_i no create pods --subresource=proxy
  can_i no create pods --subresource=exec
  can_i no get pods --subresource=log
  can_i no create pods --subresource=portforward
  can_i no get configmaps
  can_i no list configmaps

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

  # With no spec.gate the operator is v0 (D42): the window came and went with
  # no status.gate and no gate Event. And the operator as installed does not
  # set the gate's test-only knob.
  [ -z "$(field "$SERVINGDEPLOYMENT" '{.spec.gate}')" ] || die "the ServingDeployment of step 4 has a spec.gate"
  [ -z "$(field "$SERVINGDEPLOYMENT" '{.status.gate}')" ] || die "a ServingDeployment with no spec.gate has a status.gate"
  [ -z "$(gate_events)" ] || die "a ServingDeployment with no spec.gate has gate Events: $(gate_events | tr '\n' ' ')"
  knob_unset || die "deployment/operator as installed from $1 sets ${gate_knob}"
  echo "ok: with no spec.gate the window was v0: no status.gate and no gate Event; deployment/operator as installed does not set ${gate_knob}"

  echo "ok: the operator's events, oldest first:"
  k get events --field-selector involvedObject.kind=ServingDeployment \
    --sort-by=.metadata.creationTimestamp -o 'custom-columns=REASON:.reason,MESSAGE:.message' --no-headers \
    | sed 's/^/    /'
}

# --- 7. the gate in Observe --------------------------------------------------

# gate_field PATH: one field under the ServingDeployment's status.gate.
gate_field() {
  field "$SERVINGDEPLOYMENT" "{.status.gate.$1}"
}

# gate_field_set PATH: that field is set.
gate_field_set() {
  [ -n "$(gate_field "$1")" ]
}

# gate_records: status.gate's ring, oldest first, one record a line:
# INDEX|VERDICT|REASONS|STABLE_REQUESTS|CANARY_REQUESTS, the reasons as JSON.
gate_records() {
  field "$SERVINGDEPLOYMENT" \
    '{range .status.gate.records[*]}{.index}|{.verdict}|{.reasons}|{.stable.requests}|{.canary.requests}{"\n"}{end}'
}

# gate_recorded N: the ring holds at least N records.
gate_recorded() {
  [ "$(gate_records | grep -c . || true)" -ge "$1" ]
}

# record_at INDEX: the record of interval INDEX.
record_at() {
  gate_records | awk -F'|' -v i="$1" '$1 == i'
}

# canary_reads_failed_twice AFTER: two records in a row past interval AFTER
# whose canary read failed.
canary_reads_failed_twice() {
  gate_records | awk -F'|' -v after="$1" '
    $1 <= after { next }
    $3 ~ /"CanaryReadFailed"/ { if (prev != "" && $1 == prev + 1) found = 1; prev = $1; next }
    { prev = "" }
    END { exit !found }'
}

# one_operator_pod: the operator's rollout has left one pod.
one_operator_pod() {
  [ "$(k get pods -l app=operator -o name | grep -c . || true)" = 1 ]
}

# canary_exec COMMAND: run COMMAND in the canary pod's stub.
canary_exec() {
  k exec "$canary_pod" -- sh -c "$1"
}

# assert_gate: step 7, against the operator installed from the flattened
# manifests, its Lease held and steps 2 to 5 passed.
assert_gate() {
  echo "ok: --- the gate in Observe, on a ${GATE_INTERVAL} grid ---"
  local count records promote_at trip at rec verdict reasons stable_n canary_n i after rendered_spec mode

  # --- 7a. the test-only knob: shipped nowhere, set here alone ---------------

  count="$(render 03-operator.yaml | grep -cF "$gate_knob" || true)"
  [ "$count" = 0 ] || die "03-operator.yaml as apply.sh renders it sets ${gate_knob}"
  count="$("$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
    --set image.prefix="$IMAGE_PREFIX" --set image.tag="$IMAGE_TAG" | grep -cF "$gate_knob" || true)"
  [ "$count" = 0 ] || die "the chart, rendered, sets ${gate_knob}"
  echo "ok: ${gate_knob} is set neither by 03-operator.yaml as apply.sh renders it nor by the chart"

  k set env deployment/operator "${gate_knob}=${GATE_INTERVAL}" >/dev/null
  k rollout status deployment/operator --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
    || die "rollout did not converge within ${TIMEOUT_SECONDS}s: deployment/operator with ${gate_knob}"
  poll_until "the operator's earlier pod gone" one_operator_pod
  operator_pod="$(k get pods -l app=operator -o 'jsonpath={.items[0].metadata.name}')"
  poll_until "lease/${lease} held by pod/${operator_pod}" lease_held
  echo "ok: ${gate_knob}=${GATE_INTERVAL} set on this cluster's deployment/operator alone; pod/${operator_pod} holds the lease"

  # --- 7b. armed with the window; the canary held unready: pending -----------

  # The canary's init container waits on redis, so with redis at 0 no canary
  # pod can turn Ready, and every interval is pending (D45).
  k scale deployment/redis --replicas=0 >/dev/null
  poll_until "redis's pods gone" no_live_pods app=redis
  k patch "$SERVINGDEPLOYMENT" --type merge -p \
    "{\"spec\":{\"canaryImageTag\":\"${CANARY_TAG}\",\"canaryReplicas\":1,\"gate\":{\"mode\":\"Observe\"}}}" >/dev/null
  k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  poll_within "$GATE_TIMEOUT_SECONDS" "status.gate's first interval" gate_recorded 1
  [ "$(gate_field mode) $(gate_field arming) $(gate_field intervalSeconds)" = "Observe Armed ${GATE_INTERVAL%s}" ] \
    || die "status.gate runs as '$(gate_field mode) $(gate_field arming) $(gate_field intervalSeconds)'"
  records="$(gate_records)"
  if printf '%s\n' "$records" | awk -F'|' '$2 != "Pending" || $3 != "[\"CanaryNotReady\"]"' | grep -q .; then
    die "an interval recorded while no canary pod could be Ready is not pending: ${records}"
  fi
  gate_events | grep -qx GateArmed || die "no GateArmed Event"
  echo "ok: the gate armed in Observe with the window; $(printf '%s\n' "$records" | grep -c .) interval(s) pending, CanaryNotReady, while the canary waits on redis"

  # --- 7c. the canary up: warmup, then eleven clean intervals --------------

  k scale deployment/redis --replicas=1 >/dev/null
  poll_within "$GATE_TIMEOUT_SECONDS" "status.gate's promote verdict" gate_field_set promoteVerdict.interval
  promote_at="$(gate_field promoteVerdict.interval)"
  [ -z "$(gate_field rollbackVerdict.trip)" ] \
    || die "a line tripped before any breach was shaped: $(gate_field rollbackVerdict.trip)"
  [ "$(gate_records | head -n 1 | cut -d'|' -f2)" = Pending ] || die "the window's first interval is not pending"
  gate_records | awk -F'|' -v p="$promote_at" '$1 < p && $2 == "Warmup"' | grep -q . \
    || die "no warmup interval before the promote verdict"
  # The verdict's interval and the ten before it are clean: both pods read,
  # each over the floor, at runtime and under the Role that ships (D41).
  for i in $(seq $((promote_at - 10)) "$promote_at"); do
    rec="$(record_at "$i")"
    IFS='|' read -r _ verdict reasons stable_n canary_n <<<"$rec"
    [ "$verdict" = Clean ] || die "interval ${i} of the clean run reads '${rec}'"
    [ "${stable_n:-0}" -ge 50 ] && [ "${canary_n:-0}" -ge 50 ] \
      || die "interval ${i} is clean on ${stable_n:-no} stable and ${canary_n:-no} canary requests"
  done
  gate_events | grep -qx GateWouldPromote || die "no GateWouldPromote Event"
  [ "$(field "$SERVINGDEPLOYMENT" '{.status.conditions[?(@.type=="CanaryActive")].status}')" = True ] \
    || die "CanaryActive is not True after the promote verdict"
  echo "ok: warmup, then eleven clean intervals, each pod's /metrics read by the operator over the floor: the promote verdict at interval ${promote_at}, recorded, nothing dispatched"

  # --- 7d. a latency breach, two in a row ------------------------------------

  canary_pod="$(k get pods -l app=api,role=canary -o 'jsonpath={.items[0].metadata.name}')"
  [ -n "$canary_pod" ] || die "no canary pod"
  canary_exec 'echo "0 10 0" > /www/profile'
  poll_within "$GATE_TIMEOUT_SECONDS" "status.gate's rollback verdict" gate_field_set rollbackVerdict.trip
  trip="$(gate_field rollbackVerdict.trip)"
  at="$(gate_field rollbackVerdict.interval)"
  [ "$trip" = Line3CanaryLatency ] || die "the rollback verdict trips ${trip}, not Line3CanaryLatency"
  for i in $((at - 1)) "$at"; do
    rec="$(record_at "$i")"
    IFS='|' read -r _ verdict reasons _ _ <<<"$rec"
    [ "$verdict $reasons" = 'Breach ["LatencyRatio"]' ] || die "interval ${i} of the two in a row reads '${rec}'"
  done
  gate_events | grep -qx GateBreach || die "no GateBreach Event"
  gate_events | grep -qx GateWouldRollBack || die "no GateWouldRollBack Event"
  # Observe acts on nothing: the window stays open and the canary serving.
  [ "$(field "$SERVINGDEPLOYMENT" '{.status.conditions[?(@.type=="CanaryActive")].status}/{.status.conditions[?(@.type=="CanaryActive")].reason}')" = True/WindowOpen ] \
    || die "CanaryActive is not True, WindowOpen, after the rollback verdict"
  [ "$(field deployment/api-canary '{.spec.replicas}')" = 1 ] || die "deployment/api-canary is not at 1 after the rollback verdict"
  [ "$(field "pod/${canary_pod}" '{.status.phase}')" = Running ] || die "pod/${canary_pod} is not Running after the rollback verdict"
  [ "$(field "$SERVINGDEPLOYMENT" '{.spec.canaryImageTag}')" = "$CANARY_TAG" ] || die "spec.canaryImageTag moved"
  echo "ok: a slow canary breached intervals $((at - 1)) and ${at}: the rollback verdict, ${trip}, recorded; the window open and pod/${canary_pod} serving"

  # --- 7e. a canary /metrics that never answers: the reconcile unblocked ----

  after=$(($(gate_field nextBoundary) - 2))
  canary_exec 'echo "10 0 0" > /www/profile && touch /www/hang'
  poll_within "$GATE_TIMEOUT_SECONDS" "two canary reads in a row ending at the read timeout" \
    canary_reads_failed_twice "$after"
  canary_exec 'rm -f /www/hang'
  if gate_records | awk -F'|' -v a="$after" '$1 > a && $3 ~ /OperatorInterrupted/' | grep -q .; then
    die "a boundary was missed while the canary's reads hung: $(gate_records | tail -n 5 | tr '\n' ' ')"
  fi
  echo "ok: a canary /metrics that never answers costs each interval as unevaluable, CanaryReadFailed, and every boundary was still reached on time: the reconcile unblocked"

  # --- 7f. a pipeline deploy leaves spec.gate; the rendered CR has none ----

  rendered_spec="$(render 40-servingdeployment.yaml | kubectl create --dry-run=client -f - -o 'jsonpath={.spec}')"
  [ "$rendered_spec" = "{\"imageTag\":\"${IMAGE_TAG}\"}" ] || die "the rendered ServingDeployment's spec is ${rendered_spec}"
  count="$(render 40-servingdeployment.yaml | grep -cE '^[[:space:]]*gate:' || true)"
  [ "$count" = 0 ] || die "the rendered ServingDeployment carries a gate key"
  # What apply.sh sends: the CR apply, then its close-window patch.
  render 40-servingdeployment.yaml | kubectl apply -f - >/dev/null
  k patch "$SERVINGDEPLOYMENT" --type merge -p "$close_patch" >/dev/null
  k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive=false --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  mode="$(field "$SERVINGDEPLOYMENT" '{.spec.gate.mode}')"
  [ "$mode" = Observe ] || die "a pipeline deploy left spec.gate.mode '${mode}', not Observe"
  poll_until "statefulset/shadow-scorer back at 1" shadow_back
  gate_recorded 1 || die "status.gate's evidence is gone after the close"
  echo "ok: the rendered ServingDeployment's spec is imageTag alone; a pipeline deploy closed the window and left spec.gate in Observe"

  echo "ok: status.gate's ring, INDEX|VERDICT|REASONS|STABLE_REQUESTS|CANARY_REQUESTS:"
  gate_records | sed 's/^/    /'
  echo "ok: the gate's events: $(gate_events | sort | uniq -c | awk '{ printf "%s x%s; ", $2, $1 }')"
}

# --- the flattened manifests, then the chart ---------------------------------

install_flattened
assert_operator "the flattened manifests"
assert_gate
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
