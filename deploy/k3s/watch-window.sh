#!/usr/bin/env bash
#
# The watch for O4's live canary window: lines 2, 3 and 5 of the abort table in
# docs/RUNBOOK.md ("During the live canary window: close it").
#
#   deploy/k3s/watch-window.sh     # on the host, before the open patch; Ctrl-C to stop
#
# It decides nothing. Every 60 s evaluation interval it prints the values those
# lines read and a BREACH or ok mark for each, and keeps line 3's two-in-a-row
# counter. The human acts on them. On Ctrl-C it prints a session summary.
#
#   line 5, memory   /proc/meminfo sampled every 10 s; the interval's minimum
#                    MemAvailable against 409600 kB (400 MiB)
#   line 5, oom      the kernel log since the watch started (journalctl -k):
#                    any oom-kill line
#   line 2, errors   the canary's 5xx in the interval, counted once the interval
#                    starts 60 s or more after the canary pod turned Ready
#   line 3, latency  the canary's p95 against 1.25 x the stable's p95 over the
#                    same interval, with 50 /predict requests or more on each
#                    pod. An interval that cannot be evaluated is a breach.
#
# Both pods are read the same way, and Prometheus is not read at all. In a
# window the api job reaches either pod (README, "The canary split, stated
# plainly"), so no job reads the stable alone, and a canary read from
# Prometheus would not be the same measurement as the stable's. At each
# interval's end the script reads each pod's own /metrics through a
# port-forward, one function for both: the same two series, the change since
# the last read, and the p95 by histogram_quantile's interpolation over the
# bucket changes. Host memory is not in Prometheus either: the stack runs no
# node_exporter.
#
# It fails soft. A read that fails is a BREACH mark for its line, and the loop
# goes on: past its start-up checks, it never exits on its own. Its port-forwards
# use local ports 18000 (the stable) and 18001 (the canary), clear of
# smoke.sh's 9090 and 8001, so the open's smoke.sh can run beside it.
#
# Output is fixed lines; no xtrace. The sleeps pace a sampler that a human starts
# and stops. They wait on no condition, so they are not the readiness polls
# PRINCIPLES.md §5 bounds; the port-forward waits below are, and are bounded.
set -euo pipefail
set +x

NAMESPACE="${NAMESPACE:-mlobs}"
WATCH_STABLE_PORT="${WATCH_STABLE_PORT:-18000}"
WATCH_CANARY_PORT="${WATCH_CANARY_PORT:-18001}"
PORT_FORWARD_TIMEOUT_SECONDS="${PORT_FORWARD_TIMEOUT_SECONDS:-10}"

# The runbook's numbers. They are not settings.
INTERVAL_SECONDS=60
SAMPLE_SECONDS=10
MEM_FLOOR_KB=409600
REQUEST_FLOOR=50
RATIO_LIMIT=1.25
WARMUP_SECONDS=60
TRIP_RUN=2

die() {
  echo "error: $1" >&2
  exit 1
}

# --- start-up checks: before the window, so a hard exit is still safe -------

for tool in kubectl curl python3 journalctl awk; do
  command -v "$tool" >/dev/null 2>&1 || die "${tool} not found on PATH"
done
grep -q '^MemAvailable:' /proc/meminfo 2>/dev/null || die "/proc/meminfo has no MemAvailable line"
# journalctl run by a user who cannot read the system journal prints nothing
# and exits 0, which would read as "no OOM". A running host's kernel log for
# the current boot is not expected to be empty, so an empty read is taken as
# that case.
[ -n "$(journalctl -k -n 1 -q --no-pager 2>/dev/null)" ] \
  || die "the kernel log is unreadable: journalctl -k printed nothing (is this user in the adm group?)"
kubectl get namespace "$NAMESPACE" --request-timeout=10s >/dev/null 2>&1 \
  || die "kubectl cannot read namespace ${NAMESPACE}"

# --- port-forwards -------------------------------------------------------------

# Per pod, keyed stable and canary. The stable's selector also matches the
# canary, so deployment/api cannot name the stable pod: it is the one with
# app=api and no role label. The canary's carries role=canary.
declare -A pod_selector=([stable]='app=api,!role' [canary]='app=api,role=canary')
declare -A pod_port=([stable]="$WATCH_STABLE_PORT" [canary]="$WATCH_CANARY_PORT")
declare -A fwd_pid=([stable]="" [canary]="")
declare -A fwd_pod=([stable]="" [canary]="")
forward_pid=""

stop_pid() {
  [ -n "${1:-}" ] || return 0
  kill "$1" 2>/dev/null || true
  wait "$1" 2>/dev/null || true
}

stop_port_forwards() {
  stop_pid "${fwd_pid[stable]}"
  stop_pid "${fwd_pid[canary]}"
  fwd_pid[stable]=""
  fwd_pid[canary]=""
}
trap stop_port_forwards EXIT

# open_forward RESOURCE LOCAL_PORT REMOTE_PORT READY_PATH: smoke.sh's
# port_forward, made soft. Sets forward_pid, or returns 1 with nothing left
# running once READY_PATH has not answered within the bound.
open_forward() {
  local resource="$1" local_port="$2" remote_port="$3" ready_path="$4" pid deadline
  forward_pid=""
  kubectl port-forward "$resource" "${local_port}:${remote_port}" \
    --namespace "$NAMESPACE" >/dev/null 2>&1 &
  pid=$!
  deadline=$((SECONDS + PORT_FORWARD_TIMEOUT_SECONDS))
  until curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:${local_port}${ready_path}" 2>/dev/null; do
    if ! kill -0 "$pid" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
      stop_pid "$pid"
      return 1
    fi
    sleep 1
  done
  forward_pid="$pid"
}

# A forward is reused while its process lives and its path answers.
forward_alive() {
  local pid="$1" local_port="$2" ready_path="$3"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null \
    && curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:${local_port}${ready_path}" 2>/dev/null
}

# --- readers: one for both pods ------------------------------------------------

# A pod's /predict request count, its 5xx count and its cumulative duration
# buckets, as "<count> <5xx> <le>:<cumulative>,...", with "-" for the buckets
# before its first /predict.
parse_exposition='
import re, sys
count, errors, buckets = 0.0, 0.0, {}
for line in sys.stdin:
    if "endpoint=\"/predict\"" not in line:
        continue
    value = float(line.rsplit(" ", 1)[1])
    if line.startswith("mlobs_http_requests_total{"):
        count += value
        if re.search(r"status=\"5[0-9][0-9]\"", line):
            errors += value
    elif line.startswith("mlobs_http_request_duration_seconds_bucket{"):
        buckets[re.search(r"le=\"([^\"]+)\"", line).group(1)] = value
ordered = sorted(buckets.items(), key=lambda kv: float(kv[0]))
print(count, errors, ",".join(f"{le}:{c}" for le, c in ordered) or "-")
'

# pod_snapshot ROLE: sets snap to "<pod> <count> <5xx> <buckets>" for the one
# Running pod of ROLE, stable or canary, or returns 1 with snap_reason set.
snap=""
snap_reason=""
pod_snapshot() {
  local role="$1" names parsed port
  local -a pods
  snap=""
  snap_reason=""
  port="${pod_port[$role]}"
  names="$(kubectl get pods --namespace "$NAMESPACE" -l "${pod_selector[$role]}" \
      --field-selector=status.phase=Running --request-timeout=5s \
      -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" \
    || { snap_reason="the ${role} pod list is unreadable"; return 1; }
  read -r -a pods <<<"$names" || true
  if [ "${#pods[@]}" -ne 1 ]; then
    snap_reason="${#pods[@]} Running ${role} pods, not 1"
    return 1
  fi
  if [ "${pods[0]}" != "${fwd_pod[$role]}" ] \
      || ! forward_alive "${fwd_pid[$role]}" "$port" /metrics; then
    stop_pid "${fwd_pid[$role]}"
    fwd_pid[$role]=""
    fwd_pod[$role]=""
    if ! open_forward "pod/${pods[0]}" "$port" 8000 /metrics; then
      snap_reason="no port-forward to ${role} pod/${pods[0]}"
      return 1
    fi
    fwd_pid[$role]="$forward_pid"
    fwd_pod[$role]="${pods[0]}"
  fi
  parsed="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/metrics" 2>/dev/null \
      | python3 -c "$parse_exposition" 2>/dev/null)" \
    || { snap_reason="${role} pod/${pods[0]} /metrics unreadable"; return 1; }
  snap="${pods[0]} ${parsed}"
}

# ROLE and two snapshots in, "ok <count> <5xx> <p95>" or "fail <reason>" out.
# The p95 is Prometheus's histogram_quantile over the bucket changes,
# interpolation and +Inf rule included; "nan" when the interval saw no /predict.
pod_delta='
import math, sys

def parse(snapshot):
    pod, count, errors, raw = snapshot.split(" ")
    buckets = [] if raw == "-" else [
        (float(le), float(c)) for le, c in (item.rsplit(":", 1) for item in raw.split(","))]
    return pod, float(count), float(errors), buckets

def quantile(q, buckets):
    if len(buckets) < 2 or not math.isinf(buckets[-1][0]) or buckets[-1][1] <= 0:
        return math.nan
    rank = q * buckets[-1][1]
    b = next(i for i, (_, c) in enumerate(buckets) if c >= rank)
    if b == len(buckets) - 1:
        return buckets[-2][0]
    if b == 0 and buckets[0][0] <= 0:
        return buckets[0][0]
    start, below = (0.0, 0.0) if b == 0 else buckets[b - 1]
    end, c = buckets[b]
    return start + (end - start) * (rank - below) / (c - below)

role = sys.argv[1]
prev_pod, prev_count, prev_errors, prev_buckets = parse(sys.argv[2])
pod, count, errors, buckets = parse(sys.argv[3])
if pod != prev_pod:
    print(f"fail the {role} pod changed between reads")
    sys.exit(0)
before = dict(prev_buckets)
delta = [(le, c - before.get(le, 0.0)) for le, c in buckets]
if count < prev_count or errors < prev_errors or any(d < 0 for _, d in delta):
    print(f"fail a {role} counter went down between reads")
    sys.exit(0)
print("ok", round(count - prev_count), round(errors - prev_errors), quantile(0.95, delta))
'

# read_pod ROLE: the interval's read of one pod, the same for both. Takes a
# snapshot and, from the last one, sets pod_n, pod_5xx and pod_p95, or returns
# 1 with pod_reason set. The snapshot taken, or none, starts the next interval.
declare -A prev_snap=([stable]="" [canary]="")
pod_n=""
pod_5xx=""
pod_p95=""
pod_reason=""
read_pod() {
  local role="$1" result verdict rest
  pod_n=""
  pod_5xx=""
  pod_p95=""
  pod_reason=""
  if ! pod_snapshot "$role"; then
    pod_reason="$snap_reason"
    prev_snap[$role]=""
    return 1
  fi
  if [ -z "${prev_snap[$role]}" ]; then
    pod_reason="no earlier ${role} read to take the change from"
    prev_snap[$role]="$snap"
    return 1
  fi
  result="$(python3 -c "$pod_delta" "$role" "${prev_snap[$role]}" "$snap" 2>/dev/null)" \
    || result="fail the ${role} change could not be computed"
  prev_snap[$role]="$snap"
  read -r verdict rest <<<"$result" || true
  if [ "$verdict" != ok ]; then
    pod_reason="$rest"
    return 1
  fi
  read -r pod_n pod_5xx pod_p95 <<<"$rest" || true
}

# Epoch seconds of the latest Ready transition among Ready canary pods, or
# nothing when no canary pod is Ready.
canary_ready='
import datetime, json, sys
latest = None
for pod in json.load(sys.stdin).get("items", []):
    for c in pod.get("status", {}).get("conditions") or []:
        if c.get("type") == "Ready" and c.get("status") == "True":
            t = datetime.datetime.strptime(c["lastTransitionTime"], "%Y-%m-%dT%H:%M:%SZ")
            t = int(t.replace(tzinfo=datetime.timezone.utc).timestamp())
            latest = t if latest is None else max(latest, t)
print("" if latest is None else latest)
'

is_number() {
  [[ "$1" =~ ^[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?$ ]]
}

# gt A B: A > B, as floats.
gt() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'
}

fmt() {
  awk -v v="$1" -v f="$2" 'BEGIN { printf f, v + 0 }'
}

# show VALUE [FORMAT]: a number in FORMAT, anything else as read, "-" for none.
show() {
  if [ -z "$1" ]; then
    printf -- '-'
  elif [ -n "${2:-}" ] && is_number "$1"; then
    fmt "$1" "$2"
  else
    printf '%s' "$1"
  fi
}

# --- the session ---------------------------------------------------------------

session_start="$(date +%s)"
session_label="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
origin=$SECONDS
interval=0
completed=0
lat_run=0
lat_longest=0
lat_trips=""
canary_seen=0
mem_breaches=0
mem_lowest=""
oom_breaches=0
oom_seen=0
err_breaches=0
err_trips=""
lat_breaches=0
warmup_intervals=0
pending_intervals=0

summary() {
  echo
  echo "summary: ${completed} interval(s) evaluated since ${session_label}"
  echo "summary: line 5 memory: ${mem_breaches} breach(es); lowest interval minimum ${mem_lowest:-unread} kB (bar ${MEM_FLOOR_KB} kB)"
  echo "summary: line 5 oom: ${oom_breaches} breach(es); ${oom_seen} oom-kill line(s) at the last read"
  echo "summary: line 2 errors: ${err_breaches} breach(es); 5xx after warmup in interval(s): ${err_trips:-none}; ${warmup_intervals} interval(s) in warmup"
  echo "summary: line 3 latency: ${lat_breaches} breach(es); longest run ${lat_longest}; tripped at interval(s): ${lat_trips:-none}"
  echo "summary: ${pending_intervals} interval(s) before any canary pod was Ready"
}

on_interrupt() {
  trap - INT TERM
  summary
  exit 0
}
trap on_interrupt INT TERM

wait_until() {
  local now=$SECONDS
  if [ "$1" -gt "$now" ]; then sleep $(($1 - now)); fi
}

echo "ok: watching from ${session_label}: ${INTERVAL_SECONDS}s intervals, MemAvailable every ${SAMPLE_SECONDS}s, kernel log since the start; Ctrl-C stops"

# The first reads start the first interval. A canary not up yet is expected
# here: lines 2 and 3 wait for it.
for role in stable canary; do
  if pod_snapshot "$role"; then
    prev_snap[$role]="$snap"
  else
    echo "note: first read of the ${role} pod: ${snap_reason}"
  fi
done

while :; do
  interval=$((interval + 1))
  # A pod read that failed at the last boundary is retried here, so that one
  # failed read leaves one interval unevaluable rather than two.
  for role in stable canary; do
    if [ -z "${prev_snap[$role]}" ] && pod_snapshot "$role"; then prev_snap[$role]="$snap"; fi
  done
  t_start="$(date +%s)"
  label_start="$(date -u +%H:%M:%SZ)"
  boundary=$((origin + interval * INTERVAL_SECONDS))

  # Line 5, memory: six samples, one every 10 s, on a fixed grid.
  mem_min=""
  mem_samples=0
  mem_failed=0
  for k in 0 1 2 3 4 5; do
    wait_until $((boundary - INTERVAL_SECONDS + k * SAMPLE_SECONDS))
    kb="$(awk '/^MemAvailable:/ { print $2 }' /proc/meminfo 2>/dev/null)" || kb=""
    if is_number "$kb"; then
      mem_samples=$((mem_samples + 1))
      if [ -z "$mem_min" ] || [ "$kb" -lt "$mem_min" ]; then mem_min="$kb"; fi
    else
      mem_failed=$((mem_failed + 1))
    fi
  done
  wait_until "$boundary"
  label_end="$(date -u +%H:%M:%SZ)"

  # Line 5, oom: everything the kernel logged since the watch started.
  oom_lines=""
  oom_kills=""
  if [ -n "$(journalctl -k -n 1 -q --no-pager 2>/dev/null)" ] \
      && kernel_log="$(journalctl -k --since "@${session_start}" -q --no-pager 2>/dev/null)"; then
    oom_lines="$(printf '%s\n' "$kernel_log" | grep -ciE 'out of memory|oom-kill|killed process' || true)"
    oom_kills="$(printf '%s\n' "$kernel_log" | grep -ciE 'killed process' || true)"
  fi

  # Lines 2 and 3: both pods off their own /metrics, by the same function, at
  # the interval's end.
  reasons=()
  stable_n=""
  stable_5xx=""
  stable_p95=""
  if read_pod stable; then
    stable_n="$pod_n"
    stable_5xx="$pod_5xx"
    stable_p95="$pod_p95"
  else
    reasons+=("stable: ${pod_reason}")
  fi
  canary_n=""
  canary_5xx=""
  canary_p95=""
  if read_pod canary; then
    canary_n="$pod_n"
    canary_5xx="$pod_5xx"
    canary_p95="$pod_p95"
  else
    reasons+=("canary: ${pod_reason}")
  fi

  ready_read=1
  ready_epoch="$(kubectl get pods --namespace "$NAMESPACE" -l "${pod_selector[canary]}" \
      --request-timeout=5s -o json 2>/dev/null | python3 -c "$canary_ready" 2>/dev/null)" \
    || { ready_epoch=""; ready_read=0; }
  if [ -n "$ready_epoch" ]; then canary_seen=1; fi
  if [ "$ready_read" -eq 0 ]; then reasons+=("the canary pod's Ready time is unreadable"); fi

  if is_number "$stable_n" && gt "$REQUEST_FLOOR" "$stable_n"; then reasons+=("stable under the floor: ${stable_n} < ${REQUEST_FLOOR}"); fi
  if is_number "$canary_n" && gt "$REQUEST_FLOOR" "$canary_n"; then reasons+=("canary under the floor: ${canary_n} < ${REQUEST_FLOOR}"); fi
  if [ -n "$stable_p95" ] && ! is_number "$stable_p95"; then reasons+=("the stable p95 read ${stable_p95}"); fi
  if [ -n "$canary_p95" ] && ! is_number "$canary_p95"; then reasons+=("the canary p95 read ${canary_p95}"); fi

  # Compared unrounded; printed to two places.
  ratio=""
  if is_number "$canary_p95" && is_number "$stable_p95" && gt "$stable_p95" 0; then
    ratio="$(awk -v c="$canary_p95" -v s="$stable_p95" 'BEGIN { printf "%.6f", c / s }')"
  fi

  echo
  echo "interval ${interval} ${label_start}-${label_end}: minMemAvailable=$(show "$mem_min")kB (${mem_samples} samples) oomKills=$(show "$oom_kills") canary_n=$(show "$canary_n") stable_n=$(show "$stable_n") canary_p95=$(show "$canary_p95" '%.4fs') stable_p95=$(show "$stable_p95" '%.4fs') ratio=$(show "$ratio" '%.2f') canary_5xx=$(show "$canary_5xx") stable_5xx=$(show "$stable_5xx")"

  # Line 5, memory.
  if [ "$mem_failed" -gt 0 ] || [ -z "$mem_min" ]; then
    mem_breaches=$((mem_breaches + 1))
    echo "BREACH: line 5 memory: ${mem_failed} of 6 samples unreadable"
  elif [ "$mem_min" -lt "$MEM_FLOOR_KB" ]; then
    mem_breaches=$((mem_breaches + 1))
    echo "BREACH: line 5 memory: interval minimum ${mem_min} kB < ${MEM_FLOOR_KB} kB"
  else
    echo "ok: line 5 memory: interval minimum ${mem_min} kB >= ${MEM_FLOOR_KB} kB"
  fi
  if [ -n "$mem_min" ] && { [ -z "$mem_lowest" ] || [ "$mem_min" -lt "$mem_lowest" ]; }; then
    mem_lowest="$mem_min"
  fi

  # Line 5, oom.
  if [ -z "$oom_lines" ]; then
    oom_breaches=$((oom_breaches + 1))
    echo "BREACH: line 5 oom: the kernel log is unreadable"
  elif [ "$oom_lines" -gt 0 ]; then
    oom_breaches=$((oom_breaches + 1))
    oom_seen="$oom_lines"
    echo "BREACH: line 5 oom: ${oom_lines} oom line(s) in the kernel log since ${session_label}, ${oom_kills} of them 'Killed process': close at once"
  else
    echo "ok: line 5 oom: no oom line in the kernel log since ${session_label}"
  fi

  # Lines 2 and 3 start with the first Ready canary pod: before it there is no
  # window to read.
  if [ "$canary_seen" -eq 0 ]; then
    pending_intervals=$((pending_intervals + 1))
    echo "pending: line 2 errors: no canary pod has been Ready yet"
    echo "pending: line 3 latency: no canary pod has been Ready yet"
    completed=$((completed + 1))
    continue
  fi

  # Line 2, errors. The warmup runs from the latest Ready transition, so a
  # restarted canary pod gets its own. With no canary pod Ready now, nothing
  # is excluded.
  if [ "$ready_read" -eq 0 ] || ! is_number "$canary_5xx"; then
    err_breaches=$((err_breaches + 1))
    echo "BREACH: line 2 errors: unevaluable, the canary pod's 5xx count or its Ready time unread; it counts on line 3's counter"
  elif [ -n "$ready_epoch" ] && [ "$t_start" -lt $((ready_epoch + WARMUP_SECONDS)) ]; then
    warmup_intervals=$((warmup_intervals + 1))
    echo "ok: line 2 errors: warmup, the interval starts less than ${WARMUP_SECONDS}s after the canary's Ready; ${canary_5xx} 5xx not counted"
  elif gt "$canary_5xx" 0; then
    err_breaches=$((err_breaches + 1))
    err_trips="${err_trips:+${err_trips},}${interval}"
    echo "BREACH: line 2 errors: ${canary_5xx} canary 5xx after warmup: the line trips at once"
  else
    echo "ok: line 2 errors: 0 canary 5xx"
  fi

  # Line 3, latency, and the two-in-a-row counter.
  if [ "${#reasons[@]}" -gt 0 ]; then
    why="unevaluable: $(printf '%s; ' "${reasons[@]}")"
    why="${why%; }"
  elif [ -z "$ratio" ]; then
    why="unevaluable: no ratio"
  elif gt "$ratio" "$RATIO_LIMIT"; then
    why="ratio $(fmt "$ratio" '%.2f') > ${RATIO_LIMIT}"
  else
    why=""
  fi
  if [ -n "$why" ]; then
    lat_breaches=$((lat_breaches + 1))
    lat_run=$((lat_run + 1))
    if [ "$lat_run" -gt "$lat_longest" ]; then lat_longest="$lat_run"; fi
    if [ "$lat_run" -ge "$TRIP_RUN" ]; then
      lat_trips="${lat_trips:+${lat_trips},}${interval}"
      echo "BREACH: line 3 latency: ${why} (${lat_run} in a row, trips at ${TRIP_RUN}: the line has tripped)"
    else
      echo "BREACH: line 3 latency: ${why} (${lat_run} in a row, trips at ${TRIP_RUN})"
    fi
  else
    lat_run=0
    echo "ok: line 3 latency: ratio $(fmt "$ratio" '%.2f') <= ${RATIO_LIMIT} (0 in a row)"
  fi
  completed=$((completed + 1))
done
