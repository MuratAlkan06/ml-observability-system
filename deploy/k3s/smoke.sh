#!/usr/bin/env bash
#
# Post-deploy smoke check for the k3s stack — Phase 2 P2a (D25).
#
#   deploy/k3s/smoke.sh
#
# apply.sh runs this as its last step, and it is also the standalone check for a
# rollback: after `kubectl rollout undo` or a redeploy of a previous SHA, this
# script is what says the stack came back. One script, two callers (CI and the
# host), so a green CI run and a green host run mean the same thing.
#
# What it asserts, in order:
#   1. all nine workloads are rolled out
#   2. GET  /health   on the api host port answers 200
#   3. POST /predict  returns a body carrying request_id, label and confidence
#   4. every scrape job in prometheus/prometheus.yml is up, and only those
#   5. Grafana's /api/health answers 200
#
# Output is fixed lines. Nothing here reads the .env, but the same discipline
# applies: no xtrace, and failures name the check rather than echoing a payload.
set -euo pipefail
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

NAMESPACE="${NAMESPACE:-mlobs}"
# The two host ports the manifests publish (D19). Overridable so the script can
# be pointed at a tunnel rather than the box it runs on.
API_URL="${API_URL:-http://127.0.0.1:8000}"
GRAFANA_URL="${GRAFANA_URL:-http://127.0.0.1:3000}"
# Prometheus has no host port; it is reached over a port-forward that this
# script opens and closes.
PROMETHEUS_LOCAL_PORT="${PROMETHEUS_LOCAL_PORT:-9090}"

ROLLOUT_TIMEOUT_SECONDS="${ROLLOUT_TIMEOUT_SECONDS:-180}"
PORT_FORWARD_TIMEOUT_SECONDS="${PORT_FORWARD_TIMEOUT_SECONDS:-30}"
TARGETS_TIMEOUT_SECONDS="${TARGETS_TIMEOUT_SECONDS:-120}"
POLL_STEP_SECONDS="${POLL_STEP_SECONDS:-5}"

# Duplicated from apply.sh on purpose — see the note there. This script must
# work when apply.sh did not just run.
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

command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
command -v curl >/dev/null 2>&1 || die "curl not found on PATH"
# python3 is the JSON reader for steps 3 and 4. It is in the base install on
# both Ubuntu hosts this runs on; jq is not, which is why it is not used here.
command -v python3 >/dev/null 2>&1 || die "python3 not found on PATH"

# --- 1. rollouts -------------------------------------------------------------

for workload in "${WORKLOADS[@]}"; do
  if ! kubectl rollout status "$workload" \
        --namespace "$NAMESPACE" \
        --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
    die "rollout not complete within ${ROLLOUT_TIMEOUT_SECONDS}s: ${workload}"
  fi
done
echo "ok: ${#WORKLOADS[@]} workloads rolled out"

# --- 2. api /health ----------------------------------------------------------

# -f makes curl fail on any non-2xx, so a 503 `unavailable` fails here rather
# than being read as a body. 200 covers both `ok` and `degraded`, and degraded
# is a passing state by design: the model answers, Redis is the sick one.
health_code="$(curl -fsS -o /dev/null -w '%{http_code}' "${API_URL}/health")" \
  || die "GET ${API_URL}/health did not return 2xx"
[ "$health_code" = "200" ] || die "GET /health returned ${health_code}, expected 200"
echo "ok: api /health 200"

# --- 3. /predict round trip --------------------------------------------------

predict_body="$(curl -fsS -X POST "${API_URL}/predict" \
  -H 'Content-Type: application/json' \
  -d '{"text":"k3s smoke test sentence"}')" \
  || die "POST ${API_URL}/predict did not return 2xx"

# stderr is discarded and the message is fixed: the response body is model
# output, not something to splice into an error line.
if ! printf '%s' "$predict_body" | python3 -c '
import json, sys
body = json.load(sys.stdin)
required = ("request_id", "label", "confidence")
sys.exit(0 if all(field in body for field in required) else 1)
' >/dev/null 2>&1; then
  die "POST /predict response lacks one of request_id, label, confidence"
fi
echo "ok: api /predict returned request_id, label, confidence"

# --- 4. prometheus targets ---------------------------------------------------

# The expected set is read out of the scrape config rather than written down
# here, so this assertion stays true if a job is ever added or renamed — and it
# re-states D21's point: prometheus/prometheus.yml is the one description of
# what is scraped.
prometheus_config="${REPO_ROOT}/prometheus/prometheus.yml"
[ -f "$prometheus_config" ] || die "scrape config not found: ${prometheus_config}"
expected_jobs="$(grep -oE '^[[:space:]]*-[[:space:]]*job_name:[[:space:]]*[A-Za-z0-9_-]+' \
  "$prometheus_config" | awk '{print $NF}' | sort)"
# `|| true` because grep -c exits 1 on a zero count, which errexit would turn
# into a bare abort before the explanatory check below could run.
expected_count="$(printf '%s\n' "$expected_jobs" | grep -c . || true)"
[ "$expected_count" -gt 0 ] || die "no job_name entries parsed from ${prometheus_config}"

kubectl port-forward "svc/prometheus" \
  "${PROMETHEUS_LOCAL_PORT}:9090" \
  --namespace "$NAMESPACE" >/dev/null 2>&1 &
port_forward_pid=$!
trap 'kill "$port_forward_pid" 2>/dev/null || true' EXIT

prometheus_url="http://127.0.0.1:${PROMETHEUS_LOCAL_PORT}"
port_forward_deadline=$((SECONDS + PORT_FORWARD_TIMEOUT_SECONDS))
until curl -fsS -o /dev/null "${prometheus_url}/-/ready" 2>/dev/null; do
  kill -0 "$port_forward_pid" 2>/dev/null \
    || die "kubectl port-forward to prometheus exited"
  [ "$SECONDS" -lt "$port_forward_deadline" ] \
    || die "prometheus not reachable over port-forward within ${PORT_FORWARD_TIMEOUT_SECONDS}s"
  sleep 1
done

# Targets need up to one scrape_interval to report up, and the api pod may have
# become ready only moments ago, so this polls to a deadline instead of reading
# once. Bounded poll with an explicit deadline — the only sleep this repo
# sanctions (PRINCIPLES.md §5).
targets_deadline=$((SECONDS + TARGETS_TIMEOUT_SECONDS))
while :; do
  actual_jobs="$(curl -fsS "${prometheus_url}/api/v1/targets?state=active" 2>/dev/null | python3 -c '
import json, sys
payload = json.load(sys.stdin)
jobs = {t["labels"]["job"] for t in payload["data"]["activeTargets"] if t["health"] == "up"}
print("\n".join(sorted(jobs)))
' 2>/dev/null)" || actual_jobs=""
  [ "$actual_jobs" = "$expected_jobs" ] && break
  [ "$SECONDS" -lt "$targets_deadline" ] \
    || die "prometheus does not report exactly the ${expected_count} configured jobs up within ${TARGETS_TIMEOUT_SECONDS}s"
  sleep "$POLL_STEP_SECONDS"
done
echo "ok: prometheus targets up (${expected_count}/${expected_count}): $(printf '%s' "$expected_jobs" | tr '\n' ' ')"

kill "$port_forward_pid" 2>/dev/null || true
trap - EXIT

# --- 5. grafana --------------------------------------------------------------

grafana_code="$(curl -fsS -o /dev/null -w '%{http_code}' "${GRAFANA_URL}/api/health")" \
  || die "GET ${GRAFANA_URL}/api/health did not return 2xx"
[ "$grafana_code" = "200" ] || die "GET /api/health returned ${grafana_code}, expected 200"
echo "ok: grafana /api/health 200"

echo "ok: smoke passed"
