#!/usr/bin/env bash
#
# The operator's rollback across the boundary, rehearsed — Phase 3 O3
# (docs/PLAN.md D36; the host procedure is docs/RUNBOOK.md).
#
#   PRE_CUTOVER_SHA=<sha> IMAGE_TAG=<sha> CANARY_TAG=<sha> \
#     [IMAGE_PREFIX=docker.io/library] [ENV_FILE=<path>] \
#     deploy/k3s/rehearse-rollback.sh
#
# PRE_CUTOVER_SHA  a commit in this repository's history whose tree predates
#                  the operator: its 20-api.yaml still renders deployment/api
#                  and nothing in it names a ServingDeployment. It stands for
#                  the SHA the host ran before O4's cutover (issue #68).
# IMAGE_TAG        the tag this tree deploys under, as apply.sh takes it.
# CANARY_TAG       the canary window's image tag.
# ENV_FILE         the .env both apply.sh runs read. Default: a throwaway
#                  file of dummy values written here, as CI does.
#
# Runs against the current kubectl context: a fresh cluster on the pinned k3s
# image, started with the D34 pair as in K3sSmoke, with the api's node port and
# grafana's host port reachable where smoke.sh looks (API_URL, GRAFANA_URL),
# and these images already imported (CI's RollbackRehearsal job builds and
# imports them; deploy/k3s/README.md has the same steps for a laptop):
#   mlobs-{api,consumer,drift,shadow-scorer}:PRE_CUTOVER_SHA
#   mlobs-{api,consumer,drift,shadow-scorer,operator}:IMAGE_TAG
#   mlobs-api:CANARY_TAG
#
# Nothing here stands in for the pipeline. Both trees deploy through their own
# apply.sh and are checked by their own smoke.sh, as the host runs them: the
# SSM document checks a SHA out and runs that tree's apply.sh with
# IMAGE_TAG=<sha> (infra/ec2/deploy.tf). The pre-cutover tree is extracted from
# git history with `git archive`, which leaves the repository untouched.
#
# The rehearsal first puts the cluster where the host will stand after O4:
#   A. the pre-cutover tree deployed: deployment/api from its manifest, with
#      hostPort 8000 and no owner
#   B. #68's cutover, steps 2 and 3 (step 1, the D34 pair, is the cluster's
#      own flags): the hostPort patched out, then this tree deployed, so the
#      operator adopts deployment/api in place — the same object, same uid
#   C. a canary window opened by the host patch, smoke.sh green in it
# Then D36, in its order, each step asserted on what it leaves behind:
#   1. canary to 0 by host patch (apply.sh's constant close-window patch):
#      CanaryActive False, the canary's pods gone, the shadow scorer back at
#      1; smoke.sh green in the steady state
#   2. the ServingDeployment deleted while the operator still runs
#   3. deployment/api and deployment/api-canary garbage-collected through
#      their ownerReferences, waited on to a bound, and no api pod left
#   4. the operator scaled to 0; the CRD left inert, with no ServingDeployment
#      in any namespace
#   5. the pre-cutover tree deployed again by its own apply.sh: deployment/api
#      a new object from that tree's 20-api.yaml, no owner, hostPort 8000,
#      service/api a ClusterIP again, the operator still at 0
#   6. smoke.sh green: the last step of that apply.sh, then run again on its
#      own
set -euo pipefail
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

IMAGE_PREFIX="${IMAGE_PREFIX:-docker.io/library}"
PRE_CUTOVER_SHA="${PRE_CUTOVER_SHA:-}"
IMAGE_TAG="${IMAGE_TAG:-}"
CANARY_TAG="${CANARY_TAG:-}"
ENV_FILE="${ENV_FILE:-}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-180}"
# The canary's torch load is the long pole of a window opening, as in K3sSmoke.
WINDOW_STATE_TIMEOUT_SECONDS="${WINDOW_STATE_TIMEOUT_SECONDS:-300}"

NAMESPACE=mlobs
CRD_NAME=servingdeployments.serving.mlobs.dev
SERVINGDEPLOYMENT=servingdeployment/api
API_PORT=8000

# #68's step 2: the api container's hostPort removed from the adopted
# Deployment's template. The two test operations make it refuse, changing
# nothing, unless the path names the api container's port 8000 hostPort.
HOSTPORT_PATCH='[{"op":"test","path":"/spec/template/spec/containers/0/name","value":"api"},{"op":"test","path":"/spec/template/spec/containers/0/ports/0/hostPort","value":8000},{"op":"remove","path":"/spec/template/spec/containers/0/ports/0/hostPort"}]'

die() {
  echo "error: $1" >&2
  exit 1
}

[[ "$PRE_CUTOVER_SHA" =~ ^[0-9a-f]{40}$ ]] || die "PRE_CUTOVER_SHA must be a full 40-character lowercase commit SHA"
[[ "$IMAGE_TAG" =~ ^[0-9a-f]{40}$ ]] || die "IMAGE_TAG must be a 40-character lowercase hex tag"
[[ "$CANARY_TAG" =~ ^[0-9a-f]{40}$ ]] || die "CANARY_TAG must be a 40-character lowercase hex tag"
[ "$IMAGE_TAG" != "$PRE_CUTOVER_SHA" ] || die "IMAGE_TAG must differ from PRE_CUTOVER_SHA"
[ "$CANARY_TAG" != "$IMAGE_TAG" ] || die "CANARY_TAG must differ from IMAGE_TAG"
[ "$CANARY_TAG" != "$PRE_CUTOVER_SHA" ] || die "CANARY_TAG must differ from PRE_CUTOVER_SHA"
for tool in kubectl git tar; do
  command -v "$tool" >/dev/null 2>&1 || die "${tool} not found on PATH"
done

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

k() {
  kubectl --namespace "$NAMESPACE" "$@"
}

# field RESOURCE JSONPATH: one field of a namespaced object.
field() {
  k get "$1" -o "jsonpath=$2" 2>/dev/null
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

# gone RESOURCE: the API server answers, and the object is not there. A failed
# read is not taken for an absence.
gone() {
  local found
  found="$(k get "$1" --ignore-not-found -o name 2>/dev/null)" || return 1
  [ -z "$found" ]
}

# no_live_pods SELECTOR: no pod matching SELECTOR is starting or running (the
# same test as operator/hack/e2e.sh: a pod whose containers have all exited
# holds no memory, even while its object lingers).
no_live_pods() {
  local phases
  phases="$(k get pods -l "$1" -o 'jsonpath={range .items[*]}{.status.phase}{"\n"}{end}')" || return 1
  ! printf '%s\n' "$phases" | grep -qE '^(Pending|Running|Unknown)$'
}

# run_logged LOG COMMAND...: run COMMAND with its output shown and kept in LOG.
run_logged() {
  local log="$1"
  shift
  "$@" 2>&1 | tee "$log"
}

# expect_line LOG LINE WHAT: LOG holds LINE, exactly, as one of its lines.
expect_line() {
  grep -qxF "$2" "$1" || die "$3: no line '$2'"
}

# condition TYPE: STATUS/REASON of one of the ServingDeployment's conditions.
condition() {
  field "$SERVINGDEPLOYMENT" \
    "{.status.conditions[?(@.type==\"$1\")].status}/{.status.conditions[?(@.type==\"$1\")].reason}"
}

controller_of() {
  field "$1" '{.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}'
}

api_image() {
  field deployment/api '{.spec.template.spec.containers[?(@.name=="api")].image}'
}

api_host_port() {
  field deployment/api "{.spec.template.spec.containers[?(@.name==\"api\")].ports[?(@.containerPort==${API_PORT})].hostPort}"
}

# no_servingdeployments: the API server answers, and no ServingDeployment
# exists in any namespace.
no_servingdeployments() {
  local found
  found="$(kubectl get servingdeployments --all-namespaces -o name 2>/dev/null)" || return 1
  [ -z "$found" ]
}

# --- 0. the two trees, and an empty cluster ----------------------------------

git -C "$REPO_ROOT" rev-parse --verify --quiet "${PRE_CUTOVER_SHA}^{commit}" >/dev/null \
  || die "PRE_CUTOVER_SHA ${PRE_CUTOVER_SHA} is not a commit in this repository's history (a shallow clone lacks it: fetch with full depth)"

pre_tree="${work_dir}/pre-cutover"
mkdir -p "$pre_tree"
git -C "$REPO_ROOT" archive --format=tar "$PRE_CUTOVER_SHA" | tar -x -C "$pre_tree"

# The boundary D36 crosses, checked in both trees before anything is applied:
# the pre-cutover tree renders deployment/api itself and knows no
# ServingDeployment; this tree renders no deployment/api and applies the
# ServingDeployment the operator builds it from.
grep -qE '^kind: Deployment$' "${pre_tree}/deploy/k3s/manifests/20-api.yaml" \
  || die "the tree at ${PRE_CUTOVER_SHA} has no Deployment in 20-api.yaml: it is not a pre-cutover tree"
if grep -rqE '^kind: ServingDeployment$' "${pre_tree}/deploy/k3s/manifests"; then
  die "the tree at ${PRE_CUTOVER_SHA} already applies a ServingDeployment: it is not a pre-cutover tree"
fi
if grep -qE '^kind: Deployment$' "${SCRIPT_DIR}/manifests/20-api.yaml"; then
  die "this tree's 20-api.yaml still renders a Deployment: there is no boundary to cross"
fi
grep -qE '^kind: ServingDeployment$' "${SCRIPT_DIR}/manifests/40-servingdeployment.yaml" \
  || die "this tree applies no ServingDeployment: there is no boundary to cross"
echo "ok: the pre-cutover tree ${PRE_CUTOVER_SHA} renders deployment/api; this tree hands it to the operator"

if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  die "namespace ${NAMESPACE} already exists: the rehearsal starts from an empty cluster"
fi
if kubectl get crd "$CRD_NAME" >/dev/null 2>&1; then
  die "crd/${CRD_NAME} already exists: the rehearsal starts from an empty cluster"
fi

if [ -z "$ENV_FILE" ]; then
  # Dummy values, not secrets in any sense: the cluster is a throwaway one.
  # GF_ADMIN_USER is left out, as in K3sSmoke, so both trees' apply.sh take
  # their mlobsadmin default branch.
  ENV_FILE="${work_dir}/rehearsal.env"
  printf 'POSTGRES_PASSWORD=rehearsal-postgres\nGF_ADMIN_PASSWORD=rehearsal-grafana\n' > "$ENV_FILE"
fi

deploy_pre_cutover() {
  env IMAGE_PREFIX="$IMAGE_PREFIX" IMAGE_TAG="$PRE_CUTOVER_SHA" ENV_FILE="$ENV_FILE" \
    "${pre_tree}/deploy/k3s/apply.sh"
}

deploy_this_tree() {
  env IMAGE_PREFIX="$IMAGE_PREFIX" IMAGE_TAG="$IMAGE_TAG" ENV_FILE="$ENV_FILE" \
    "${REPO_ROOT}/deploy/k3s/apply.sh"
}

# --- A. the pre-cutover tree deployed ----------------------------------------

run_logged "${work_dir}/A-apply.log" deploy_pre_cutover \
  || die "setup A: the pre-cutover tree's apply.sh failed"
expect_line "${work_dir}/A-apply.log" 'ok: smoke passed' "setup A: the pre-cutover apply.sh"

adopted_uid="$(field deployment/api '{.metadata.uid}')"
[ -n "$adopted_uid" ] || die "setup A: deployment/api not found"
[ -z "$(field deployment/api '{.metadata.ownerReferences}')" ] \
  || die "setup A: deployment/api has an owner before the cutover"
[ "$(api_host_port)" = "$API_PORT" ] || die "setup A: deployment/api carries no hostPort ${API_PORT}"
[ "$(api_image)" = "${IMAGE_PREFIX}/mlobs-api:${PRE_CUTOVER_SHA}" ] \
  || die "setup A: deployment/api runs $(api_image), not the pre-cutover tag"
echo "ok: setup A: the pre-cutover tree deployed by its own apply.sh; deployment/api from its manifest, hostPort ${API_PORT}, no owner"

# --- B. #68's cutover, steps 2 and 3: the operator adopts deployment/api -----

k patch deployment/api --type json -p "$HOSTPORT_PATCH" >/dev/null \
  || die "setup B: the hostPort patch was refused"
k rollout status deployment/api --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "setup B: deployment/api did not roll out without its hostPort within ${TIMEOUT_SECONDS}s"
[ -z "$(api_host_port)" ] || die "setup B: deployment/api still carries a hostPort"
echo "ok: setup B: hostPort ${API_PORT} patched out of deployment/api (#68 step 2)"

run_logged "${work_dir}/B-apply.log" deploy_this_tree \
  || die "setup B: this tree's apply.sh failed"
expect_line "${work_dir}/B-apply.log" 'ok: close-window patch sent; no canary window was open' "setup B: this tree's apply.sh"
expect_line "${work_dir}/B-apply.log" 'ok: smoke passed (steady state)' "setup B: this tree's apply.sh"

[ "$(field deployment/api '{.metadata.uid}')" = "$adopted_uid" ] \
  || die "setup B: deployment/api is a new object, not the pre-cutover one adopted in place"
[ "$(controller_of deployment/api)" = "ServingDeployment/api" ] \
  || die "setup B: deployment/api is controlled by '$(controller_of deployment/api)', not ServingDeployment/api"
[ "$(api_image)" = "${IMAGE_PREFIX}/mlobs-api:${IMAGE_TAG}" ] \
  || die "setup B: deployment/api runs $(api_image), not this tree's tag"
adopted_events="$(k get events --field-selector involvedObject.kind=ServingDeployment,reason=Adopted -o name)" \
  || die "setup B: could not read the ServingDeployment's events"
[ -n "$adopted_events" ] || die "setup B: no Adopted event on the ServingDeployment"
echo "ok: setup B: this tree deployed (#68 step 3); the operator adopted deployment/api in place (uid ${adopted_uid} kept) at $(api_image)"

# --- C. a canary window, open --------------------------------------------------

k patch "$SERVINGDEPLOYMENT" --type merge \
  -p "{\"spec\":{\"canaryImageTag\":\"${CANARY_TAG}\",\"canaryReplicas\":1}}" >/dev/null
k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "setup C: CanaryActive not True within ${TIMEOUT_SECONDS}s"
k wait "$SERVINGDEPLOYMENT" --for=condition=ShadowPaused --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "setup C: ShadowPaused not True within ${TIMEOUT_SECONDS}s"
run_logged "${work_dir}/C-smoke.log" \
  env STATE_TIMEOUT_SECONDS="$WINDOW_STATE_TIMEOUT_SECONDS" "${REPO_ROOT}/deploy/k3s/smoke.sh" \
  || die "setup C: smoke.sh failed in the window"
expect_line "${work_dir}/C-smoke.log" 'ok: smoke passed (window state)' "setup C: smoke.sh"
echo "ok: setup C: a canary window opened by the host patch at ${CANARY_TAG}; smoke green in the window state"

# --- D36 step 1: canary to 0 by host patch -----------------------------------

# The close is apply.sh's constant, read from apply.sh rather than copied: the
# host patch and the pipeline's are one patch (D32).
close_patch="$(sed -n "s/^CLOSE_WINDOW_PATCH='\(.*\)'\$/\1/p" "${SCRIPT_DIR}/apply.sh")"
[ -n "$close_patch" ] || die "CLOSE_WINDOW_PATCH not found in deploy/k3s/apply.sh"
k patch "$SERVINGDEPLOYMENT" --type merge -p "$close_patch" >/dev/null
k wait "$SERVINGDEPLOYMENT" --for=condition=CanaryActive=false --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "D36 step 1: CanaryActive not False within ${TIMEOUT_SECONDS}s"
shadow_back() {
  [ "$(field statefulset/shadow-scorer '{.spec.replicas}')" = 1 ]
}
poll_until "D36 step 1: statefulset/shadow-scorer back at 1" shadow_back
k rollout status statefulset/shadow-scorer --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "D36 step 1: statefulset/shadow-scorer not rolled out within ${TIMEOUT_SECONDS}s"
k wait "$SERVINGDEPLOYMENT" --for=condition=ShadowPaused=false --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "D36 step 1: ShadowPaused not False within ${TIMEOUT_SECONDS}s"
[ "$(condition CanaryActive)" = "False/NoCanary" ] \
  || die "D36 step 1: CanaryActive is $(condition CanaryActive), not False/NoCanary"
[ "$(field deployment/api-canary '{.spec.replicas}')" = 0 ] || die "D36 step 1: deployment/api-canary is not at 0"
no_live_pods app=api,role=canary || die "D36 step 1: a canary pod still runs"
run_logged "${work_dir}/1-smoke.log" "${REPO_ROOT}/deploy/k3s/smoke.sh" \
  || die "D36 step 1: smoke.sh failed after the close"
expect_line "${work_dir}/1-smoke.log" 'ok: smoke passed (steady state)' "D36 step 1: smoke.sh"
echo "ok: D36 step 1: canary to 0 by the host patch: CanaryActive $(condition CanaryActive), api-canary at 0 with no pods, shadow scorer at 1, ShadowPaused $(condition ShadowPaused); smoke green, steady"

# --- D36 step 2: delete the ServingDeployment, the operator still running -----

[ "$(field deployment/operator '{.status.readyReplicas}')" = 1 ] \
  || die "D36 step 2: the operator is not running; D36 deletes the CR while it runs"
[ "$(controller_of deployment/api)" = "ServingDeployment/api" ] \
  || die "D36 step 2: deployment/api is not controlled by ServingDeployment/api before the delete"
k delete "$SERVINGDEPLOYMENT" --timeout="${TIMEOUT_SECONDS}s" >/dev/null \
  || die "D36 step 2: deleting ${SERVINGDEPLOYMENT} did not finish within ${TIMEOUT_SECONDS}s"
gone "$SERVINGDEPLOYMENT" || die "D36 step 2: ${SERVINGDEPLOYMENT} still exists"
echo "ok: D36 step 2: ${SERVINGDEPLOYMENT} deleted while the operator ran"

# --- D36 step 3: deployment/api garbage-collected, to a bound ----------------

gc_started=$SECONDS
poll_until "D36 step 3: deployment/api garbage-collected" gone deployment/api
poll_until "D36 step 3: deployment/api-canary garbage-collected" gone deployment/api-canary
poll_until "D36 step 3: no api pod left" no_live_pods app=api
echo "ok: D36 step 3: deployment/api and deployment/api-canary garbage-collected through their ownerReferences, no api pod left, in $((SECONDS - gc_started))s (bound ${TIMEOUT_SECONDS}s each)"

# --- D36 step 4: the operator down, the CRD left inert ------------------------

k scale deployment/operator --replicas=0 >/dev/null
poll_until "D36 step 4: no operator pod left" no_live_pods app=operator
kubectl get crd "$CRD_NAME" >/dev/null 2>&1 || die "D36 step 4: crd/${CRD_NAME} is gone; it is left inert"
no_servingdeployments || die "D36 step 4: a ServingDeployment exists"
# Nothing re-created the api while the operator was still up.
gone deployment/api || die "D36 step 4: deployment/api exists again"
echo "ok: D36 step 4: the operator scaled to 0 with no pod left; crd/${CRD_NAME} left inert, no ServingDeployment in any namespace; deployment/api still gone"

# --- D36 step 5: the pre-cutover SHA through the pipeline ---------------------

run_logged "${work_dir}/5-apply.log" deploy_pre_cutover \
  || die "D36 step 5: the pre-cutover tree's apply.sh failed"
expect_line "${work_dir}/5-apply.log" 'ok: smoke passed' "D36 step 5: the pre-cutover apply.sh"

rolled_back_uid="$(field deployment/api '{.metadata.uid}')"
[ -n "$rolled_back_uid" ] || die "D36 step 5: deployment/api not found"
[ "$rolled_back_uid" != "$adopted_uid" ] || die "D36 step 5: deployment/api is the adopted object, not a new one"
[ -z "$(field deployment/api '{.metadata.ownerReferences}')" ] \
  || die "D36 step 5: deployment/api has an owner after the rollback"
[ "$(api_host_port)" = "$API_PORT" ] || die "D36 step 5: deployment/api carries no hostPort ${API_PORT}"
[ "$(api_image)" = "${IMAGE_PREFIX}/mlobs-api:${PRE_CUTOVER_SHA}" ] \
  || die "D36 step 5: deployment/api runs $(api_image), not the pre-cutover tag"
[ "$(field service/api '{.spec.type}')" = ClusterIP ] \
  || die "D36 step 5: service/api is $(field service/api '{.spec.type}'), not ClusterIP"
[ "$(field deployment/operator '{.spec.replicas}')" = 0 ] || die "D36 step 5: the operator is no longer at 0"
no_servingdeployments || die "D36 step 5: a ServingDeployment exists"
echo "ok: D36 step 5: the pre-cutover tree redeployed by its own apply.sh; deployment/api a new object (uid ${rolled_back_uid}) from its 20-api.yaml, no owner, hostPort ${API_PORT}; service/api ClusterIP; the operator still at 0"

# --- D36 step 6: smoke.sh green ----------------------------------------------

run_logged "${work_dir}/6-smoke.log" "${pre_tree}/deploy/k3s/smoke.sh" \
  || die "D36 step 6: the pre-cutover smoke.sh failed standalone"
expect_line "${work_dir}/6-smoke.log" 'ok: smoke passed' "D36 step 6: smoke.sh"
echo "ok: D36 step 6: the pre-cutover smoke.sh green, as the last step of its apply.sh and again standalone"

# What the rollback leaves in place, all of it inert: none of it selects a pod
# or runs one. Listed so a reader of the log sees it rather than infers it.
echo "ok: left in place, inert:"
kubectl get "crd/${CRD_NAME}" -o name | sed 's/^/    /'
k get deployment/operator service/api-canary serviceaccount/operator role/operator rolebinding/operator \
  -o name --ignore-not-found | sed 's/^/    /'
echo "ok: D36 rollback rehearsal passed"
