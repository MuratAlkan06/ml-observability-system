#!/usr/bin/env bash
#
# Post-deploy smoke check for the k3s stack — Phase 2 P2a (D25), state-aware
# since Phase 3 O2 (D37).
#
#   deploy/k3s/smoke.sh
#
# apply.sh runs this as its last step, and it is also the standalone check for a
# rollback: after `kubectl rollout undo` or a redeploy of a previous SHA, this
# script is what says the stack came back. One script, two callers (CI and the
# host), so a green CI run and a green host run mean the same thing.
#
# The stack is in exactly one of two states (D37), and this script decides
# which from the cluster alone: the ServingDeployment's CanaryActive and
# ShadowPaused conditions at its current generation, and the canary Deployment
# and the shadow scorer's StatefulSet as observed. It never reads the spec's
# canary fields, which an expired window leaves behind until the close-window
# patch clears them (the D32 clarification of O2), so an expired-but-unclosed
# spec is steady here like any other.
#   steady  CanaryActive False and ShadowPaused False; deployment/api-canary
#           absent or at 0 with no pods; statefulset/shadow-scorer at 1, ready
#   window  CanaryActive True and ShadowPaused True; statefulset/shadow-scorer
#           at 0 with no pods; deployment/api-canary at 1 or more, every
#           replica updated and ready
# Anything else — a transition still settling, or a state put together by
# hand — is polled to a deadline and then rejected.
#
# What it asserts, in order:
#   1. the stack is in exactly one of the two states
#   2. the state's workloads are rolled out: the nine, plus
#      deployment/api-canary in a window
#   3. GET  /health on the api's node port answers 200
#   4. POST /predict returns a body carrying request_id, label and confidence;
#      in a window, a batch of connections through the same port reaches the
#      canary as well (the D34 split)
#   5. Prometheus reports exactly the state's scrape jobs up: every job in
#      prometheus/prometheus.yml, less api_canary when steady and less
#      shadow_scorer in a window
#   6. Grafana's /api/health answers 200
#
# Output is fixed lines. Nothing here reads the .env, but the same discipline
# applies: no xtrace, and failures name the check rather than echoing a payload.
set -euo pipefail
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

NAMESPACE="${NAMESPACE:-mlobs}"
# The api's node port (D34) and Grafana's host port (D19). Overridable so the
# script can be pointed at a tunnel rather than the box it runs on.
API_URL="${API_URL:-http://127.0.0.1:8000}"
GRAFANA_URL="${GRAFANA_URL:-http://127.0.0.1:3000}"
# Prometheus has no host port, and the canary's metrics are read off its own
# Service; both are reached over port-forwards this script opens and closes.
PROMETHEUS_LOCAL_PORT="${PROMETHEUS_LOCAL_PORT:-9090}"
CANARY_LOCAL_PORT="${CANARY_LOCAL_PORT:-8001}"

ROLLOUT_TIMEOUT_SECONDS="${ROLLOUT_TIMEOUT_SECONDS:-180}"
STATE_TIMEOUT_SECONDS="${STATE_TIMEOUT_SECONDS:-180}"
PORT_FORWARD_TIMEOUT_SECONDS="${PORT_FORWARD_TIMEOUT_SECONDS:-30}"
TARGETS_TIMEOUT_SECONDS="${TARGETS_TIMEOUT_SECONDS:-120}"
POLL_STEP_SECONDS="${POLL_STEP_SECONDS:-5}"
# Connections sent through API_URL in a window to find the canary among them.
# With one stable and one canary pod each connection lands on either with
# even odds, so twenty all missing the canary is a one-in-a-million event.
SPLIT_CONNECTIONS="${SPLIT_CONNECTIONS:-20}"

SERVINGDEPLOYMENT=servingdeployment/api
CANARY=deployment/api-canary
SHADOW=statefulset/shadow-scorer
# The two scrape jobs whose presence depends on the state (D37).
CANARY_JOB=api_canary
SHADOW_JOB=shadow_scorer

# Duplicated from apply.sh on purpose — see the note there. This script must
# work when apply.sh did not just run.
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

die() {
  echo "error: $1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
command -v curl >/dev/null 2>&1 || die "curl not found on PATH"
# python3 is the JSON reader for steps 1, 4 and 5. It is in the base install
# on both Ubuntu hosts this runs on; jq is not, which is why it is not used here.
command -v python3 >/dev/null 2>&1 || die "python3 not found on PATH"

# Port-forwards opened below, killed on any exit.
port_forward_pids=()
stop_port_forwards() {
  local pid
  # The ${...+...} form expands an empty array to nothing under `set -u` on
  # every bash, the macOS 3.2 of the local rehearsal included.
  for pid in ${port_forward_pids[@]+"${port_forward_pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
    # Reaped here, quietly, so the shell prints no job-termination notice.
    wait "$pid" 2>/dev/null || true
  done
  port_forward_pids=()
}
trap stop_port_forwards EXIT

# port_forward RESOURCE LOCAL_PORT REMOTE_PORT READY_PATH: open a port-forward
# and wait, to a deadline, for READY_PATH to answer through it.
port_forward() {
  local resource="$1" local_port="$2" remote_port="$3" ready_path="$4" pid deadline
  kubectl port-forward "$resource" "${local_port}:${remote_port}" \
    --namespace "$NAMESPACE" >/dev/null 2>&1 &
  pid=$!
  port_forward_pids+=("$pid")
  deadline=$((SECONDS + PORT_FORWARD_TIMEOUT_SECONDS))
  until curl -fsS -o /dev/null "http://127.0.0.1:${local_port}${ready_path}" 2>/dev/null; do
    kill -0 "$pid" 2>/dev/null || die "kubectl port-forward to ${resource} exited"
    [ "$SECONDS" -lt "$deadline" ] \
      || die "${resource} not reachable over port-forward within ${PORT_FORWARD_TIMEOUT_SECONDS}s"
    sleep 1
  done
}

# --- 1. state ----------------------------------------------------------------

# Reads the three objects as one kubectl List on stdin and prints `steady`,
# `window`, or `neither: <what was observed>`. The observation is conditions
# and replica counts only, so it is safe to print on a failure line.
classify_state='
import json, sys

doc = json.load(sys.stdin)
items = doc.get("items", [doc] if doc.get("kind") else [])
found = {item["kind"]: item for item in items}
sd, sts, canary = found.get("ServingDeployment"), found.get("StatefulSet"), found.get("Deployment")
if sd is None:
    print("neither: servingdeployment/api not found")
    sys.exit(0)
if sts is None:
    print("neither: statefulset/shadow-scorer not found")
    sys.exit(0)

generation = sd["metadata"].get("generation")
status = sd.get("status", {})
current = status.get("observedGeneration") == generation
conditions = {c["type"]: c for c in status.get("conditions", [])}

def condition(kind):
    c = conditions.get(kind)
    if c is None or c.get("observedGeneration") != generation:
        return "Stale"
    return c.get("status", "Unknown")

def replicas(obj):
    """(spec, pods, ready, updated, settled) as the workload reports them."""
    if obj is None:
        return 0, 0, 0, 0, True
    st = obj.get("status", {})
    spec = obj["spec"].get("replicas", 1)
    # A Deployment leaves terminating pods out of status.replicas and counts
    # them apart; a StatefulSet counts them in it.
    pods = st.get("replicas", 0) + (st.get("terminatingReplicas") or 0)
    settled = st.get("observedGeneration", 0) >= obj["metadata"].get("generation", 0)
    return spec, pods, st.get("readyReplicas", 0), st.get("updatedReplicas", 0), settled

active, paused = condition("CanaryActive"), condition("ShadowPaused")
s_spec, s_pods, s_ready, _, s_settled = replicas(sts)
c_spec, c_pods, c_ready, c_updated, c_settled = replicas(canary)

steady = (current and active == "False" and paused == "False"
          and c_spec == 0 and c_pods == 0 and c_settled
          and s_spec == 1 and s_ready == 1 and s_settled)
window = (current and active == "True" and paused == "True"
          and s_spec == 0 and s_pods == 0 and s_settled
          and c_spec >= 1 and c_pods == c_spec and c_ready == c_spec and c_updated == c_spec and c_settled)
freshness = "current" if current else "behind"
if steady:
    print("steady")
elif window:
    print("window")
else:
    print(
        f"neither: CanaryActive={active} ShadowPaused={paused} "
        f"status {freshness} at generation {generation}, "
        f"shadow-scorer {s_ready}/{s_spec} ready of {s_pods} pod(s), "
        f"api-canary {c_ready}/{c_spec} ready of {c_pods} pod(s)"
    )
'

state_deadline=$((SECONDS + STATE_TIMEOUT_SECONDS))
while :; do
  state="$(kubectl get "$SERVINGDEPLOYMENT" "$SHADOW" "$CANARY" \
      --namespace "$NAMESPACE" --ignore-not-found -o json 2>/dev/null \
    | python3 -c "$classify_state" 2>/dev/null)" || state="neither: servingdeployment/api unreadable"
  case "$state" in
    steady | window) break ;;
  esac
  [ "$SECONDS" -lt "$state_deadline" ] \
    || die "the stack is in neither the steady nor the window state within ${STATE_TIMEOUT_SECONDS}s (D37): ${state#neither: }"
  sleep "$POLL_STEP_SECONDS"
done
case "$state" in
  steady) echo "ok: stack state steady: CanaryActive False, ShadowPaused False, no canary pods, shadow scorer at 1" ;;
  window) echo "ok: stack state window: CanaryActive True, ShadowPaused True, canary serving, shadow scorer at 0" ;;
esac

# --- 2. rollouts -------------------------------------------------------------

workloads=("${WORKLOADS[@]}")
[ "$state" = window ] && workloads+=("$CANARY")
for workload in "${workloads[@]}"; do
  if ! kubectl rollout status "$workload" \
        --namespace "$NAMESPACE" \
        --timeout="${ROLLOUT_TIMEOUT_SECONDS}s" >/dev/null 2>&1; then
    die "rollout not complete within ${ROLLOUT_TIMEOUT_SECONDS}s: ${workload}"
  fi
done
echo "ok: ${#workloads[@]} workloads rolled out"

# --- 3. api /health ----------------------------------------------------------

# -f makes curl fail on any non-2xx, so a 503 `unavailable` fails here rather
# than being read as a body. 200 covers both `ok` and `degraded`, and degraded
# is a passing state by design: the model answers, Redis is the sick one.
health_code="$(curl -fsS -o /dev/null -w '%{http_code}' "${API_URL}/health")" \
  || die "GET ${API_URL}/health did not return 2xx"
[ "$health_code" = "200" ] || die "GET /health returned ${health_code}, expected 200"
echo "ok: api /health 200"

# --- 4. /predict round trip --------------------------------------------------

predict() {
  curl -fsS -X POST "${API_URL}/predict" \
    -H 'Content-Type: application/json' \
    -d '{"text":"k3s smoke test sentence"}'
}

predict_body="$(predict)" || die "POST ${API_URL}/predict did not return 2xx"

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

# In a window the canary sits behind the same Service as the stable, and the
# split is per connection (D34). Its predictions counter, read off its own
# Service before and after a batch of connections through API_URL, shows
# whether any of them reached it. Each curl is a new connection. Other
# traffic can only add to the count, never hide the canary's share of it.
canary_predictions() {
  curl -fsS "http://127.0.0.1:${CANARY_LOCAL_PORT}/metrics" 2>/dev/null | python3 -c '
import sys
total = 0.0
for line in sys.stdin:
    if line.startswith("mlobs_predictions_total{"):
        total += float(line.rsplit(" ", 1)[1])
print(int(total))
' 2>/dev/null
}

if [ "$state" = window ]; then
  port_forward "svc/api-canary" "$CANARY_LOCAL_PORT" 8000 /metrics
  before="$(canary_predictions)" || die "could not read the canary's predictions counter"
  for _ in $(seq 1 "$SPLIT_CONNECTIONS"); do
    predict >/dev/null || die "POST ${API_URL}/predict did not return 2xx"
  done
  after="$(canary_predictions)" || die "could not read the canary's predictions counter"
  stop_port_forwards
  reached=$((after - before))
  [ "$reached" -ge 1 ] \
    || die "none of ${SPLIT_CONNECTIONS} /predict connections through ${API_URL} reached the canary"
  echo "ok: the canary served ${reached} of ${SPLIT_CONNECTIONS} /predict connections through ${API_URL} (D34 split)"
fi

# --- 5. prometheus targets ---------------------------------------------------

# The configured set is read out of the scrape config rather than written down
# here, so this assertion stays true if a job is ever added or renamed — and it
# re-states D21's point: prometheus/prometheus.yml is the one description of
# what is scraped. The state then removes the one job it leaves without
# endpoints: api_canary when steady, shadow_scorer in a window. What remains
# must be up, and nothing else may be.
prometheus_config="${REPO_ROOT}/prometheus/prometheus.yml"
[ -f "$prometheus_config" ] || die "scrape config not found: ${prometheus_config}"
configured_jobs="$(grep -oE '^[[:space:]]*-[[:space:]]*job_name:[[:space:]]*[A-Za-z0-9_-]+' \
  "$prometheus_config" | awk '{print $NF}' | sort)"
for job in "$CANARY_JOB" "$SHADOW_JOB"; do
  printf '%s\n' "$configured_jobs" | grep -qxF "$job" \
    || die "${prometheus_config} has no job ${job}, which the state-aware target set names (D37)"
done
case "$state" in
  steady) absent_job="$CANARY_JOB" ;;
  window) absent_job="$SHADOW_JOB" ;;
esac
# `|| true` because grep exits 1 on a zero count or no match, which errexit
# would turn into a bare abort before the explanatory check below could run.
expected_jobs="$(printf '%s\n' "$configured_jobs" | grep -vxF "$absent_job" || true)"
expected_count="$(printf '%s\n' "$expected_jobs" | grep -c . || true)"
[ "$expected_count" -gt 0 ] || die "no job_name entries parsed from ${prometheus_config}"

port_forward "svc/prometheus" "$PROMETHEUS_LOCAL_PORT" 9090 /-/ready
prometheus_url="http://127.0.0.1:${PROMETHEUS_LOCAL_PORT}"

# Targets need up to one scrape_interval to report up — or down, for the job
# the state removes — and the api pod may have become ready only moments ago,
# so this polls to a deadline instead of reading once. Bounded poll with an
# explicit deadline — the only sleep this repo sanctions (PRINCIPLES.md §5).
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
    || die "prometheus does not report exactly the ${expected_count} jobs of the ${state} state up within ${TARGETS_TIMEOUT_SECONDS}s (${absent_job} must be down)"
  sleep "$POLL_STEP_SECONDS"
done
echo "ok: prometheus targets up for the ${state} state (${expected_count}, ${absent_job} down): $(printf '%s' "$expected_jobs" | tr '\n' ' ')"

stop_port_forwards

# --- 6. grafana --------------------------------------------------------------

grafana_code="$(curl -fsS -o /dev/null -w '%{http_code}' "${GRAFANA_URL}/api/health")" \
  || die "GET ${GRAFANA_URL}/api/health did not return 2xx"
[ "$grafana_code" = "200" ] || die "GET /api/health returned ${grafana_code}, expected 200"
echo "ok: grafana /api/health 200"

echo "ok: smoke passed (${state} state)"
