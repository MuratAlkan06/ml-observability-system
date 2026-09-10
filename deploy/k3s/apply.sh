#!/usr/bin/env bash
#
# Deploy the ML Observability stack to a k3s (or k3d) cluster — Phase 2 P2a.
#
#   IMAGE_TAG=<tag> [IMAGE_PREFIX=<registry/owner>] [ENV_FILE=<path>] deploy/k3s/apply.sh
#
# IMAGE_PREFIX  registry + owner for the four mlobs images. Default
#               ghcr.io/muratalkan06 (the per-SHA / :main tags GhcrPublish
#               pushes). CI overrides it to docker.io/library, which is how
#               containerd names an image imported from a local build.
# IMAGE_TAG     required, no default — deploying "whatever :latest happens to
#               mean" is the failure mode this whole slice exists to remove.
# ENV_FILE      the host .env holding POSTGRES_PASSWORD, GF_ADMIN_PASSWORD and
#               optionally GF_ADMIN_USER / SLACK_WEBHOOK_URL. Default <repo>/.env.
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

# The nine workloads this stack deploys. smoke.sh carries the same list and
# waits on it again, deliberately: smoke.sh has to stand alone as the post-
# deploy and rollback check (D25), and a check that only works when apply.sh
# just ran is not a check. Touch one list, touch the other.
WORKLOADS=(
  deployment/postgres
  deployment/redis
  deployment/api
  deployment/prometheus
  deployment/grafana
  deployment/drift
  deployment/drift-shadow
  statefulset/consumer
  statefulset/shadow-scorer
)

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

# --- 0. preflight ------------------------------------------------------------

[ -n "$IMAGE_TAG" ] || die "IMAGE_TAG is required (for example IMAGE_TAG=\$(git rev-parse HEAD))"
command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
[ -f "$ENV_FILE" ] || die "env file not found: ${ENV_FILE}"

# One scratch directory for everything this run generates, removed on any exit.
# Nothing secret is written into it: the env file is handed to kubectl by path
# and never copied.
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# --- 1. namespace ------------------------------------------------------------

# Applied on its own and first: the Secret and ConfigMaps below are created
# into this namespace and cannot be if it does not exist yet.
kubectl apply -f "${SCRIPT_DIR}/manifests/00-namespace.yaml" >/dev/null
echo "ok: namespace ${NAMESPACE} applied"

# --- 2. secret ---------------------------------------------------------------

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

# --- 3. config maps ----------------------------------------------------------

# Every ConfigMap is generated from the canonical file in the repository. There
# is deliberately no copy of prometheus.yml, the Grafana provisioning tree, the
# dashboards, the SQL or the baselines under deploy/ — one source, two runtimes
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

# --- 4. render manifests (image refs + prometheus config hash) ---------------

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

kubectl apply --namespace "$NAMESPACE" -f "$render_dir" >/dev/null
echo "ok: manifests applied (images ${IMAGE_PREFIX}/mlobs-*:${IMAGE_TAG})"
echo "ok: prometheus config-hash ${config_hash}"

# --- 5. wait for the rollouts ------------------------------------------------

for workload in "${WORKLOADS[@]}"; do
  if ! kubectl rollout status "$workload" \
        --namespace "$NAMESPACE" \
        --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
    die "rollout did not converge within ${ROLLOUT_TIMEOUT_SECONDS}s: ${workload}"
  fi
done
echo "ok: ${#WORKLOADS[@]} workloads rolled out"

# --- 6. smoke ----------------------------------------------------------------

# Run, not exec: exec would replace this process and the EXIT trap that removes
# the render directory would never fire. errexit propagates the exit status
# either way, so a failing smoke fails the deploy.
NAMESPACE="$NAMESPACE" "${SCRIPT_DIR}/smoke.sh"
