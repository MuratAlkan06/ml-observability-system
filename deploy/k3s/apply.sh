#!/usr/bin/env bash
#
# Deploy the ML Observability stack to a k3s (or k3d) cluster — Phase 2 P2a,
# with the operator's deploy sequence from Phase 3 O2 (docs/PLAN.md D32).
#
#   IMAGE_TAG=<sha> [IMAGE_PREFIX=<registry/owner>] [ENV_FILE=<path>] deploy/k3s/apply.sh
#
# IMAGE_PREFIX  registry + owner for the five mlobs images. Default
#               ghcr.io/muratalkan06 (the per-SHA / :main tags GhcrPublish
#               pushes). CI overrides it to docker.io/library, which is how
#               containerd names an image imported from a local build.
# IMAGE_TAG     required, no default — deploying "whatever :latest happens to
#               mean" is the failure mode this whole slice exists to remove.
#               A full 40-character lowercase commit SHA: it becomes the
#               ServingDeployment's spec.imageTag, whose schema admits nothing
#               else.
# ENV_FILE      the host .env holding POSTGRES_PASSWORD, GF_ADMIN_PASSWORD and
#               optionally GF_ADMIN_USER / SLACK_WEBHOOK_URL. Default <repo>/.env.
#
# The order is D32's and is not a matter of taste: every manifest is rendered,
# k3s's recorded node-port range is read and the api Service is dry-run
# server-side before the cluster is touched (the preflight, step 2); then the
# namespace, the Secret and the ConfigMaps; the CRD, waited on until
# Established; the operator, waited on until rolled out; the rest of the
# stack; the ServingDeployment; the constant close-window patch, which can
# close a canary window and can never open one; the ServingDeployment's Ready
# condition at its current generation; the rollouts, deployment/api first; and
# smoke.sh.
#
# Secret hygiene contract (D23). This script reads a file full of credentials,
# so three rules hold throughout and are worth stating because each one is easy
# to break by accident:
#   1. xtrace is never enabled — see the `set +x` below.
#   2. No secret value is ever read into a shell variable, passed as a process
#      argument, or interpolated into a string. The .env is handed to kubectl
#      as a path; its contents are checked with grep, which reports only
#      whether a pattern matched.
#   3. Anything that could echo secret material has its output discarded, and
#      failures are reported as fixed lines that contain no input.
set -euo pipefail
# Defensive, not decorative: `bash -x deploy/k3s/apply.sh` would otherwise
# print every expanded command, and `kubectl create secret ... -o yaml` is one
# of them. Turning xtrace off here makes rule 1 hold no matter how the script
# was invoked.
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

IMAGE_PREFIX="${IMAGE_PREFIX:-ghcr.io/muratalkan06}"
IMAGE_TAG="${IMAGE_TAG:-}"
ENV_FILE="${ENV_FILE:-${REPO_ROOT}/.env}"
NAMESPACE="${NAMESPACE:-mlobs}"
ROLLOUT_TIMEOUT_SECONDS="${ROLLOUT_TIMEOUT_SECONDS:-180}"

# The nine workloads of the stack, deployment/api first: D32's sequence waits
# on it right after the ServingDeployment's Ready. smoke.sh carries the same
# list and waits on it again, deliberately: smoke.sh has to stand alone as the
# post-deploy and rollback check (D25), and a check that only works when
# apply.sh just ran is not a check. Touch one list, touch the other. The
# operator's own rollout is waited on where the sequence applies it, and the
# canary, deployment/api-canary, is not in this list: it runs only inside a
# canary window, which this script closes, and smoke.sh adds it when it finds
# the stack in a window.
WORKLOADS=(
  deployment/api
  deployment/postgres
  deployment/redis
  deployment/prometheus
  deployment/grafana
  deployment/drift
  deployment/drift-shadow
  statefulset/consumer
  statefulset/shadow-scorer
)

# The files D32's sequence applies one by one, in its own order. Every other
# file under manifests/ is applied together as "the stack".
NAMESPACE_MANIFEST=00-namespace.yaml
CRD_MANIFEST=01-servingdeployment-crd.yaml
CRD_NAME=servingdeployments.serving.mlobs.dev
OPERATOR_MANIFESTS=(02-operator-rbac.yaml 03-operator.yaml)
API_SERVICE_MANIFEST=20-api.yaml
SERVINGDEPLOYMENT_MANIFEST=40-servingdeployment.yaml
SERVINGDEPLOYMENT=servingdeployment/api

# The close-window patch (D32): a constant, never built from input. It clears
# both canary fields, which closes a window that is open and is a no-op on a
# spec that holds none; nothing in this script can set them.
CLOSE_WINDOW_PATCH='{"spec":{"canaryImageTag":null,"canaryReplicas":0}}'

die() {
  echo "error: $1" >&2
  exit 1
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# --- 0. local checks ---------------------------------------------------------

[ -n "$IMAGE_TAG" ] || die "IMAGE_TAG is required (for example IMAGE_TAG=\$(git rev-parse HEAD))"
# The ServingDeployment's spec.imageTag pattern, checked here so that a short
# SHA or a moving tag fails before anything is applied rather than at the CR.
[[ "$IMAGE_TAG" =~ ^[0-9a-f]{40}$ ]] \
  || die "IMAGE_TAG must be a full 40-character lowercase commit SHA (for example IMAGE_TAG=\$(git rev-parse HEAD))"
command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
[ -f "$ENV_FILE" ] || die "env file not found: ${ENV_FILE}"

# One scratch directory for everything this run generates, removed on any exit.
# Nothing secret is written into it: the env file is handed to kubectl by path
# and never copied.
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# --- 1. render manifests (image refs + prometheus config hash) ---------------

# Rendering touches nothing in the cluster, so it runs before anything does,
# and the preflight below dry-runs what was rendered.
#
# The config hash is substituted in the same pass as the image refs rather than
# patched onto a live Deployment afterwards. Applying PLACEHOLDER and then
# patching would change the pod template twice and roll Prometheus twice on
# every deploy; substituting first means the object that reaches the cluster is
# already final, and an unchanged config produces a byte-identical pod template
# and therefore no rollout at all.
config_hash="$(sha256_of "${REPO_ROOT}/prometheus/prometheus.yml")"

# Inside the scratch directory created in step 0, so it is covered by the same
# EXIT trap. A second `trap ... EXIT` here would REPLACE that one, not add to
# it, and the generated env file would survive the run.
render_dir="${work_dir}/manifests"
mkdir -p "$render_dir"

for manifest in "${SCRIPT_DIR}"/manifests/*.yaml; do
  sed -e "s|IMAGE_PREFIX|${IMAGE_PREFIX}|g" \
      -e "s|IMAGE_TAG|${IMAGE_TAG}|g" \
      -e "s|PLACEHOLDER|${config_hash}|g" \
      "$manifest" > "${render_dir}/$(basename "$manifest")"
done

# A placeholder that survives rendering would deploy an unresolvable image ref
# or a constant annotation that never forces a rollout. Both fail quietly, so
# they are caught loudly here instead.
if grep -rlE 'IMAGE_PREFIX|IMAGE_TAG|PLACEHOLDER' "$render_dir" >/dev/null 2>&1; then
  die "unsubstituted placeholder left in the rendered manifests"
fi

# --- 2. preflight: the node-port range admits 8000 (D34) ---------------------

# The api Service is a NodePort on 8000, which a cluster admits only when k3s
# runs with the D34 flag pair, --service-node-port-range=8000-8000 together
# with --disable-network-policy (deploy/k3s/README.md, "Host k3s flags"). On a
# cluster without it the deploy must stop here, before it changes anything,
# with the stack exactly as it was rather than half-applied. Two reads, neither
# of which writes:
#
# 1. The range itself, from k3s's own record of how it was started: every
#    k3s node carries a k3s.io/node-args annotation holding its arguments,
#    flags from /etc/rancher/k3s/config.yaml included. A server-side dry-run
#    cannot answer this question: dry-run node-port allocation checks only
#    whether the port is taken, not whether it is in range, so a default-range
#    cluster passes it (observed on the pinned k3s v1.36.4; a real create is
#    refused). A server node whose recorded range does not admit 8000 — no
#    range recorded means the default, 30000-32767 — stops the deploy on the
#    fixed line below. The annotation is k3s's, which is what D19 pins, and it
#    is re-read on any k3s bump with the rest of D34's pin sites. A cluster
#    whose server nodes carry no such annotation is not k3s: its range cannot
#    be read this way, so a warning says so and the dry-run stands alone.
# 2. The rendered Service, dry-run server-side, so a Service the API server
#    would refuse for any other reason also stops the deploy here. The
#    namespace may not exist yet, and creating it would be a change, so on a
#    fresh cluster the same Service is dry-run into `default`.
#
# Nothing captured here comes from the env file: node annotations and a
# Service's validation errors.
NODE_PORT=8000
NODE_PORT_RANGE_FAILURE="preflight: the cluster's node-port range does not admit ${NODE_PORT}; start k3s with the D34 pair --service-node-port-range=8000-8000 and --disable-network-policy, never one without the other. Nothing was applied."
# The flag the preflight reads: one half of the D34 pair. The other half,
# --disable-network-policy, is not read: a k3s given the exact range without
# it crash-loops (the D34 erratum of O2), which no preflight needs to report.
RANGE_FLAG=--service-node-port-range

# range_admits RANGE: whether a value of the range flag admits NODE_PORT. The
# three notations the API server's flag parser accepts are read: low-high,
# base+offset and a single port. Anything else admits nothing.
range_admits() {
  local low high
  case "$1" in
    *[!0-9+-]* | '') return 1 ;;
    *-*) low="${1%%-*}" high="${1#*-}" ;;
    *+*) low="${1%%+*}" high="${1#*+}" ;;
    *) low="$1" high="$1" ;;
  esac
  [[ "$low" =~ ^[0-9]+$ && "$high" =~ ^[0-9]+$ ]] || return 1
  case "$1" in *+*) high=$((low + high)) ;; esac
  [ "$low" -le "$NODE_PORT" ] && [ "$NODE_PORT" -le "$high" ]
}

node_args="$(kubectl get nodes \
  -o jsonpath='{range .items[*]}{.metadata.annotations.k3s\.io/node-args}{"\n"}{end}' 2>/dev/null)" \
  || die "preflight: could not read the cluster's nodes. Nothing was applied."
server_args="$(printf '%s\n' "$node_args" | grep -F '["server"' || true)"
if [ -z "$server_args" ]; then
  echo "warning: preflight: no server node carries a k3s.io/node-args annotation, so this is not k3s and its node-port range cannot be read; the server-side dry-run below does not check it (D34)" >&2
  node_port_range="unread"
else
  while IFS= read -r args; do
    # The flag's last occurrence wins, as it does when k3s parses it; none at
    # all is k3s's default range, which does not admit 8000. The annotation
    # records the flag either as "--flag=value" or as "--flag","value".
    recorded="$(printf '%s\n' "$args" \
      | grep -oE "\"${RANGE_FLAG}(=|\",\")[^\"]*\"" | tail -n 1 \
      | sed -E "s/^\"${RANGE_FLAG}(=|\",\")//; s/\"\$//")" || recorded=""
    range_admits "${recorded:-30000-32767}" || die "$NODE_PORT_RANGE_FAILURE"
    node_port_range="$recorded"
  done <<<"$server_args"
fi

preflight_manifest="${render_dir}/${API_SERVICE_MANIFEST}"
preflight_namespace="$NAMESPACE"
if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
  preflight_namespace=default
  preflight_manifest="${work_dir}/preflight-${API_SERVICE_MANIFEST}"
  sed -e "s|^  namespace: ${NAMESPACE}\$|  namespace: ${preflight_namespace}|" \
    "${render_dir}/${API_SERVICE_MANIFEST}" > "$preflight_manifest"
fi
if ! kubectl apply --dry-run=server --namespace "$preflight_namespace" \
      -f "$preflight_manifest" >/dev/null 2>&1; then
  die "preflight: server-side dry-run of service/api was refused. Nothing was applied; to see why, re-run: kubectl apply --dry-run=server -f deploy/k3s/manifests/${API_SERVICE_MANIFEST}"
fi
echo "ok: preflight: node-port range ${node_port_range}; service/api accepted by a server-side dry-run"

# --- 3. namespace ------------------------------------------------------------

# The first change this script makes, once the preflight has passed. Applied on
# its own: the Secret and ConfigMaps below are created into this namespace and
# cannot be if it does not exist yet.
kubectl apply -f "${render_dir}/${NAMESPACE_MANIFEST}" >/dev/null
echo "ok: namespace ${NAMESPACE} applied"

# --- 4. secret ---------------------------------------------------------------

# Presence-and-non-empty check by pattern match only. `..+` requires at least
# three characters after the `=`, which rejects both the missing key and the
# `KEY=` empty line without ever learning what the value is.
require_env_key() {
  grep -qE "^$1=..+" "$ENV_FILE" \
    || die "${ENV_FILE} must set $1 to a non-empty value"
}
require_env_key POSTGRES_PASSWORD
require_env_key GF_ADMIN_PASSWORD

secret_args=(--from-env-file="$ENV_FILE")
if grep -qE '^GF_ADMIN_USER=' "$ENV_FILE"; then
  # Present. It must also be usable: an empty value would reach Grafana as an
  # empty admin login rather than falling back to anything.
  grep -qE '^GF_ADMIN_USER=..+' "$ENV_FILE" \
    || die "${ENV_FILE} sets GF_ADMIN_USER to an empty value — give it a name, or delete the line to take the mlobsadmin default"
else
  # Compose wrote this default as `${GF_ADMIN_USER:-mlobsadmin}`. A manifest has
  # no shell-style default, so the default is materialised into the Secret here
  # and the manifest's secretKeyRef stays required.
  #
  # A second --from-env-file rather than --from-literal: kubectl rejects
  # `--from-env-file` combined with `--from-file` or `--from-literal` outright,
  # but the flag itself repeats. The generated file holds a username and no
  # secret, so writing it out costs nothing.
  printf 'GF_ADMIN_USER=mlobsadmin\n' > "${work_dir}/gf-admin-user.env"
  secret_args+=(--from-env-file="${work_dir}/gf-admin-user.env")
fi

# `create --dry-run=client -o yaml | apply -f -` is the idempotent-create idiom:
# `create` alone fails on the second run, and `apply` alone cannot read an env
# file. The YAML in the middle of that pipe holds every secret value in base64,
# so it goes straight into the next process and nowhere else.
#
# BOTH halves are silenced, stderr included. `create` is the half that parses the
# env file, so it is the half that quotes a line back at you when the parse
# fails — silencing only `apply` would leave the leak open on exactly the path
# most likely to hit it.
if ! kubectl create secret generic mlobs-secrets \
      --namespace "$NAMESPACE" "${secret_args[@]}" \
      --dry-run=client -o yaml 2>/dev/null \
    | kubectl apply --namespace "$NAMESPACE" -f - >/dev/null 2>&1; then
  die "failed to apply secret mlobs-secrets. kubectl output is suppressed because it quotes lines from the env file; to see it, re-run: kubectl create secret generic mlobs-secrets --namespace ${NAMESPACE} --from-env-file=<your env file> --dry-run=client -o yaml >/dev/null"
fi
echo "ok: secret applied"

# --- 5. config maps ----------------------------------------------------------

# Every ConfigMap is generated from the canonical file in the repository. There
# is deliberately no copy of prometheus.yml, the Grafana provisioning tree, the
# dashboards, the SQL or the baselines under deploy/ — one source of truth
# (D21). Output is discarded here too: not secret, just noise that would bury
# the fixed lines.
apply_configmap() {
  local name="$1"
  shift
  if ! kubectl create configmap "$name" --namespace "$NAMESPACE" "$@" \
        --dry-run=client -o yaml \
      | kubectl apply --namespace "$NAMESPACE" -f - >/dev/null 2>&1; then
    die "failed to apply configmap ${name}"
  fi
  echo "ok: configmap ${name} applied"
}

apply_configmap prometheus-config \
  --from-file=prometheus.yml="${REPO_ROOT}/prometheus/prometheus.yml"

apply_configmap grafana-provisioning-datasources \
  --from-file="${REPO_ROOT}/grafana/provisioning/datasources"

apply_configmap grafana-provisioning-dashboards \
  --from-file="${REPO_ROOT}/grafana/provisioning/dashboards"

apply_configmap grafana-dashboards \
  --from-file="${REPO_ROOT}/grafana/dashboards"

# The postgres entrypoint runs /docker-entrypoint-initdb.d in lexical order, and
# the natural filenames sort wrong: `002_shadow.sql` would run before
# `init.sql`, and its ALTER TABLE would hit a drift_runs that does not exist.
# Numbering the ConfigMap keys puts init.sql first and the migrations after it
# in their own order, for however many migrations there eventually are.
initdb_args=(--from-file=000-init.sql="${REPO_ROOT}/sql/init.sql")
migration_index=1
for migration in "${REPO_ROOT}"/sql/migrations/*.sql; do
  [ -e "$migration" ] || continue
  initdb_args+=(--from-file="$(printf '%03d' "$migration_index")-$(basename "$migration")=${migration}")
  migration_index=$((migration_index + 1))
done
apply_configmap postgres-initdb "${initdb_args[@]}"

# baseline/ also holds sst2_validation.tsv, which is build-time input for
# scripts/build_baseline.py; only the committed baseline artifacts are mounted.
baseline_args=()
for baseline in "${REPO_ROOT}"/baseline/*.json; do
  [ -e "$baseline" ] || continue
  baseline_args+=(--from-file="$(basename "$baseline")=${baseline}")
done
[ "${#baseline_args[@]}" -gt 0 ] || die "no baseline JSON found under ${REPO_ROOT}/baseline"
apply_configmap drift-baseline "${baseline_args[@]}"

# --- 6. the CRD, then the operator (D32) -------------------------------------

# The CRD first, alone, and Established before anything names its kind: an
# apply of the ServingDeployment against a CRD the API server has not yet
# served fails with "no matches for kind".
kubectl apply -f "${render_dir}/${CRD_MANIFEST}" >/dev/null
if ! kubectl wait --for=condition=Established "crd/${CRD_NAME}" \
      --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
  die "crd/${CRD_NAME} not Established within ${ROLLOUT_TIMEOUT_SECONDS}s"
fi
echo "ok: crd/${CRD_NAME} applied and Established"

operator_args=()
for manifest in "${OPERATOR_MANIFESTS[@]}"; do
  operator_args+=(-f "${render_dir}/${manifest}")
done
kubectl apply --namespace "$NAMESPACE" "${operator_args[@]}" >/dev/null
if ! kubectl rollout status deployment/operator \
      --namespace "$NAMESPACE" \
      --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
  die "rollout did not converge within ${ROLLOUT_TIMEOUT_SECONDS}s: deployment/operator"
fi
echo "ok: operator applied and rolled out"

# --- 7. the stack ------------------------------------------------------------

# Everything else under manifests/: the Services, the workloads other than the
# api, which the operator owns, and the observability pair.
stack_args=()
for manifest in "${render_dir}"/*.yaml; do
  case "$(basename "$manifest")" in
    "$NAMESPACE_MANIFEST" | "$CRD_MANIFEST" | "$SERVINGDEPLOYMENT_MANIFEST") continue ;;
  esac
  for operator_manifest in "${OPERATOR_MANIFESTS[@]}"; do
    [ "$(basename "$manifest")" = "$operator_manifest" ] && continue 2
  done
  stack_args+=(-f "$manifest")
done
kubectl apply --namespace "$NAMESPACE" "${stack_args[@]}" >/dev/null
echo "ok: manifests applied (images ${IMAGE_PREFIX}/mlobs-*:${IMAGE_TAG})"
echo "ok: prometheus config-hash ${config_hash}"

# --- 8. the ServingDeployment, then the close-window patch (D32) -------------

kubectl apply --namespace "$NAMESPACE" -f "${render_dir}/${SERVINGDEPLOYMENT_MANIFEST}" >/dev/null
echo "ok: ${SERVINGDEPLOYMENT} applied (imageTag ${IMAGE_TAG})"

# The canary fields as they stood before the patch decide which fixed line the
# patch prints, and nothing else: the patch is sent either way. A spec that
# held a window's fields — an open window, or one the operator already closed
# at its TTL and left in the spec — is cleared on the line below; a pipeline
# deploy during an open window closes it (D32).
if ! canary_before="$(kubectl get "$SERVINGDEPLOYMENT" --namespace "$NAMESPACE" \
      -o jsonpath='{.spec.canaryImageTag}/{.spec.canaryReplicas}' 2>/dev/null)"; then
  die "could not read ${SERVINGDEPLOYMENT} before the close-window patch"
fi
if ! kubectl patch "$SERVINGDEPLOYMENT" --namespace "$NAMESPACE" \
      --type merge -p "$CLOSE_WINDOW_PATCH" >/dev/null 2>&1; then
  die "close-window patch on ${SERVINGDEPLOYMENT} failed"
fi
case "$canary_before" in
  / | /0) echo "ok: close-window patch sent; no canary window was open" ;;
  *) echo "ok: canary window closed by the close-window patch (D32)" ;;
esac

# --- 9. the ServingDeployment Ready at its current generation ----------------

# Ready is the operator's word that deployment/api has rolled out spec.imageTag
# and is Available (the D37 addendum of O1). It is only an answer for this
# deploy when it was computed for the generation this deploy and the patch
# left: status.observedGeneration and the condition's own observedGeneration
# must both equal metadata.generation. A bounded poll with an explicit
# deadline, the only sleep this repo sanctions (PRINCIPLES.md §5).
ready_jsonpath='{.metadata.generation} {.status.observedGeneration}'
ready_jsonpath+=' {.status.conditions[?(@.type=="Ready")].status}'
ready_jsonpath+=' {.status.conditions[?(@.type=="Ready")].observedGeneration}'
ready_deadline=$((SECONDS + ROLLOUT_TIMEOUT_SECONDS))
while :; do
  ready_state="$(kubectl get "$SERVINGDEPLOYMENT" --namespace "$NAMESPACE" \
    -o jsonpath="$ready_jsonpath" 2>/dev/null)" || ready_state=""
  read -r generation observed ready ready_generation <<<"$ready_state" || true
  if [ -n "${generation:-}" ] && [ "$generation" = "${observed:-}" ] \
      && [ "${ready:-}" = "True" ] && [ "${ready_generation:-}" = "$generation" ]; then
    break
  fi
  [ "$SECONDS" -lt "$ready_deadline" ] \
    || die "${SERVINGDEPLOYMENT} not Ready at its current generation within ${ROLLOUT_TIMEOUT_SECONDS}s"
  sleep 2
done
echo "ok: ${SERVINGDEPLOYMENT} Ready at generation ${generation}"

# --- 10. wait for the rollouts -----------------------------------------------

for workload in "${WORKLOADS[@]}"; do
  if ! kubectl rollout status "$workload" \
        --namespace "$NAMESPACE" \
        --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
    die "rollout did not converge within ${ROLLOUT_TIMEOUT_SECONDS}s: ${workload}"
  fi
done
echo "ok: ${#WORKLOADS[@]} workloads rolled out"

# --- 11. smoke ---------------------------------------------------------------

# Run, not exec: exec would replace this process and the EXIT trap that removes
# the render directory would never fire. errexit propagates the exit status
# either way, so a failing smoke fails the deploy.
NAMESPACE="$NAMESPACE" "${SCRIPT_DIR}/smoke.sh"
