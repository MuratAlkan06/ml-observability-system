#!/usr/bin/env bash
#
# The ephemeral EKS demonstration — Phase 3 H2 (docs/PLAN.md D39). Owner-run,
# from a laptop, as the designated non-root admin principal. Never CI: no
# workflow runs this and no path filter names deploy/eks/.
#
#   AWS_PROFILE=<profile> deploy/eks/demo.sh run <stable-sha> <canary-sha>
#   AWS_PROFILE=<profile> deploy/eks/demo.sh sweep <cluster-name>
#   AWS_PROFILE=<profile> deploy/eks/demo.sh teardown <cluster-name>
#
# run       the whole demonstration, below. <stable-sha> and <canary-sha> are
#           two different 40-hex commits on main whose images GhcrPublish has
#           pushed: the operator and the api run at the first, and the canary
#           window opens on the second. `git fetch origin` first: both are
#           checked against the local origin/main.
# sweep     the orphan sweep alone, for the T+24h evidence line (D39).
#           <cluster-name> is the mlobs-demo-<nonce> the run printed.
# teardown  teardown-first from any state: eksctl delete cluster --wait, then
#           the sweep. For a run that ended without its own teardown — a killed
#           shell, a laptop that slept through the trap.
#
# Requires aws (v2), kubectl, curl, jq, git and tar on PATH. helm and eksctl
# are not taken from PATH: the script fetches the pinned releases below into a
# scratch directory, checks each against its release checksum and asserts its
# version on a fixed line, as CI does for helm and k3d (D38, D39).
#
# What `run` does, in order, each step on a fixed line, with the kubectl
# output under it:
#   1. the identity: `aws sts get-caller-identity`, the transcript's first
#      line, refused if it is the account root (D13, D24, D29)
#   2. the pinned helm and eksctl, checksum-verified, versions asserted
#   3. same-day verification: EKS 1.36 in standard support in us-west-2, and
#      the $0.10/h standard-support control-plane rate from the AWS Price List
#   4. both SHAs on main, and the run's three GHCR pulls preflighted with
#      deploy.yml's anonymous-token manifest check: the operator and the api at
#      the stable SHA, the api at the canary SHA
#   5. cluster create from deploy/eks/cluster.yaml, rendered for a per-run
#      name, into a scratch kubeconfig. The T-clock starts here, and from here
#      on every exit — a failure, an interrupt, the hard T+3h bound — tears the
#      cluster down first.
#   6. the node Ready with a public IPv4 address; the 1.36 API server
#   7. 00-namespace.yaml and 11-redis.yaml applied VERBATIM from
#      deploy/k3s/manifests: the api's redis-ready init container needs redis
#   8. the chart installed with helm into mlobs at the stable SHA; the operator
#      Ready and holding its Lease
#   9. 40-servingdeployment.yaml, rendered as apply.sh renders it, applied:
#      deployment/api created, controlled by it, at the rendered image, Ready
#      at observedGeneration == generation
#  10. /health and /predict through a port-forward to deployment/api
#  11. the T+2h valve: past T+2h from create, the window segment (12-14) is
#      cut and the demo goes to teardown (D39's pre-decided de-scope line)
#  12. a window opened with the canary patch: CanaryActive True at the current
#      generation, deployment/api-canary at the canary image
#  13. the canary observed serving, pod-scoped: /predict 200 through a
#      port-forward to deployment/api-canary, and the canary's own
#      mlobs_predictions_total higher after it; ShadowPaused False with reason
#      ShadowNotFound, recorded as expected — no shadow scorer exists on EKS
#  14. apply.sh's constant close patch, then steady
#  15. the D36-shaped teardown: the ServingDeployment deleted, deployment/api
#      garbage-collected through its ownerReference, helm uninstall, the CRD
#      deleted explicitly and last (crds/ is install-only, D38), eksctl delete
#      cluster --wait, the scratch kubeconfig deleted; then the orphan sweep,
#      the infra/ec2 plan backstop, the cost line and the T+24h instruction.
#
# What it is not: smoke.sh. smoke.sh's state classifier needs the shadow
# StatefulSet and the nine-workload stack, neither of which exists here, so
# these assertions are the demo's own. On EKS no svc/api or svc/api-canary
# exists and a port-forward pins one pod, so the D34 per-connection split and
# the NodePort are stated, not demonstrated: they are k3s properties, proven by
# K3sSmoke and the live host (D39).
#
# The bound is time (D39). T0 is the moment `eksctl create cluster` starts. At
# T+2h the window segment is cut (step 11). At T+3h a watchdog stops the demo
# wherever it is and the trap tears down first; every wait in the demo is also
# capped at what is left before T+3h, so no step can outlast it by more than a
# poll. A teardown, once started, ignores INT and TERM.
#
# Output is fixed lines: `ok:`, `fail:`, `error:` (a refusal before anything
# exists), `skip:`, `valve:`, `note:`, `cost:` and `next:`; tool output is
# indented under them. Nothing here reads a secret. The GHCR token is an
# anonymous pull token and is never printed; the terraform plan's output,
# which carries the SSH ingress CIDR, goes to a scratch file and is never
# printed either, as in CI's TerraformPlan.
#
# Two test seams exist for deploy/eks/test/, and the transcript says so on a
# `note:` line when either is set, so a harness run cannot pass for evidence:
#   DEMO_TEST_CLOCK_FILE  a file holding the epoch seconds now() returns
#   DEMO_TEST_TOOLS_DIR   a directory holding helm and eksctl, used in place
#                         of the pinned downloads
set -euo pipefail
set -E
set +x

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
MANIFESTS="${REPO_ROOT}/deploy/k3s/manifests"
CHART="${REPO_ROOT}/deploy/helm/mlobs-operator"
CLUSTER_CONFIG="${SCRIPT_DIR}/cluster.yaml"

# --- pins (D38, D39) ----------------------------------------------------------

# Helm: the H1 pin. HELM_VERSION and HELM_SHA256 (linux-amd64) are
# byte-identical to the pin in .github/workflows/operator.yml and ci.yml, and
# move with it. The other platforms' checksums are the same release's, from
# get.helm.sh/helm-v3.22.0-<platform>.tar.gz.sha256sum.
HELM_VERSION=v3.22.0
HELM_SHA256=1e4ab49e429626cf6c6958d914248b78c9730803c2751b87627e171dc800e7bb

helm_sha256() {
  case "$1" in
    linux-amd64) echo "$HELM_SHA256" ;;
    linux-arm64) echo f14e804dfee240f55525b667488fe9adca349e63e00c9af634c0beb1421ac310 ;;
    darwin-amd64) echo bd1d09f316558dda23698527859600936fed1371021ce0f2272d66b2fdcfa69c ;;
    darwin-arm64) echo 4c9982a6cdeb458b60258df66b55398ca5b19293f6877faffe2909ad6f23dfe0 ;;
    *) return 1 ;;
  esac
}

# eksctl: exact, on D39's 0.230.x line, fixed at H2 start (2026-10-05):
# v0.230.0, the line's only release. Checksums from the release's own
# eksctl_checksums.txt. eksctl has read Kubernetes version support from EKS's
# DescribeClusterVersions since v0.202.0, so 1.36 needs no newer build.
EKSCTL_VERSION=0.230.0

eksctl_sha256() {
  case "$1" in
    Linux_amd64) echo a2060956f117c3065abafda5c1f681679b9c3716675d70ce4ffff46033b02c35 ;;
    Linux_arm64) echo 21afe8a1e38f0e8153a1f27ff7af6b90e309a0411a1438139463dac2f866674d ;;
    Darwin_amd64) echo 9c169be56572dae079dc1e5e2a6efff83c4cc6fc8507e54d0a6e8f4ef14df312 ;;
    Darwin_arm64) echo 1412b7ea32efab8141c4c7ccdf96690814d659accefdf72e4e6277ea5c87470c ;;
    *) return 1 ;;
  esac
}

# --- the demonstration's fixed facts (D39) -----------------------------------

REGION=us-west-2
EKS_VERSION=1.36
# The standard-support control-plane rate D39's cost arithmetic uses, checked
# on the day against the AWS Price List's public offer file for AmazonEKS in
# us-west-2 (credential-free), at the per-cluster usage type.
EKS_RATE_USD=0.10
PRICE_LIST_URL=https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEKS/current/us-west-2/index.json
EKS_USAGE_TYPE=USW2-AmazonEKS-Hours:perCluster
# D39's arithmetic, not a measurement: the control plane plus the t3.medium.
HOURLY_ARITHMETIC_USD=0.145

GHCR_OWNER=muratalkan06
IMAGE_PREFIX="ghcr.io/${GHCR_OWNER}"
GHCR_ACCEPT='application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json'

NAMESPACE=mlobs
RELEASE=mlobs-operator
CRD_NAME=servingdeployments.serving.mlobs.dev
SERVINGDEPLOYMENT=servingdeployment/api
CLUSTER_NAME_PATTERN='^mlobs-demo-[0-9a-f]{8}$'

VALVE_SECONDS=7200
HARD_BOUND_SECONDS=10800
EKSCTL_TIMEOUT=40m

# A demo step's wait. The long pole is the api's first Ready: its image bakes
# torch and a model (DockerBuild holds it under 2GB) and a fresh node pulls it
# cold, then the startup probe allows 180 s. Every wait is also capped at what
# is left before T+3h (step_timeout).
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-900}"
TEARDOWN_TIMEOUT_SECONDS="${TEARDOWN_TIMEOUT_SECONDS:-180}"
POLL_SECONDS="${POLL_SECONDS:-3}"
WATCHDOG_POLL_SECONDS="${WATCHDOG_POLL_SECONDS:-15}"
STABLE_LOCAL_PORT="${STABLE_LOCAL_PORT:-18000}"
CANARY_LOCAL_PORT="${CANARY_LOCAL_PORT:-18001}"

# The AWS CLI pages long output through `less` by default, which would stall
# an unattended run.
export AWS_PAGER=""

# --- state ---------------------------------------------------------------------

MODE=""
CLUSTER_NAME=""
STABLE_SHA=""
CANARY_SHA=""
TMP_ROOT="${TMPDIR:-/tmp}"
TMP_ROOT="${TMP_ROOT%/}"
SCRATCH=""
KUBECONFIG_PATH=""
HELM=""
EKSCTL=""
T0=""
VALVE_AT=""
HARD_DEADLINE=""
DELETED_AT=""
create_started=0
torn_down=0
valve_used=0
watchdog_pid=""
last_err_line=""
sd_generation=""
teardown_failures=0
sweep_failures=0
sweep_lines=0
port_forward_pids=()

# --- output ---------------------------------------------------------------------

# A refusal before anything exists.
die() {
  echo "error: $1" >&2
  exit 1
}

# A check that failed. After create, the EXIT trap tears down first.
fail() {
  echo "fail: $1" >&2
  exit 1
}

indent() {
  sed 's/^/    /'
}

# show_file FILE: a response body, indented, with a final newline.
show_file() {
  if [ -s "$1" ]; then
    printf '%s\n' "$(cat "$1")" | indent
  fi
}

usage() {
  cat >&2 <<'EOF'
usage: AWS_PROFILE=<profile> deploy/eks/demo.sh run <stable-sha> <canary-sha>
       AWS_PROFILE=<profile> deploy/eks/demo.sh sweep <mlobs-demo-nonce>
       AWS_PROFILE=<profile> deploy/eks/demo.sh teardown <mlobs-demo-nonce>
EOF
  exit 2
}

# --- the clock ------------------------------------------------------------------

now() {
  if [ -n "${DEMO_TEST_CLOCK_FILE:-}" ]; then
    cat "$DEMO_TEST_CLOCK_FILE"
  else
    date +%s
  fi
}

utc_of() {
  date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ
}

# elapsed: the time since T0 as T+<h>h<mm>m, or the UTC time before there is
# a T0 (the sweep and teardown modes have none).
elapsed() {
  local s
  if [ -z "$T0" ]; then
    utc_of "$(now)"
    return
  fi
  s=$(($(now) - T0))
  [ "$s" -ge 0 ] || s=0
  printf 'T+%dh%02dm' $((s / 3600)) $(((s % 3600) / 60))
}

# checkpoint STEP: the hard bound, checked before STEP. Past T+3h the demo
# stops here and the EXIT trap tears down first. Never called in a subshell:
# its exit has to end the script.
checkpoint() {
  [ -n "$HARD_DEADLINE" ] || return 0
  if [ "$(now)" -ge "$HARD_DEADLINE" ]; then
    fail "T+3h hard bound reached before: $1 ($(elapsed)); teardown first (D39)"
  fi
}

# step_timeout: TIMEOUT_SECONDS, or what is left before the hard bound when
# that is less, so that no wait outlasts T+3h.
step_timeout() {
  local left
  if [ -z "$HARD_DEADLINE" ]; then
    echo "$TIMEOUT_SECONDS"
    return
  fi
  left=$((HARD_DEADLINE - $(now)))
  [ "$left" -ge 1 ] || left=1
  if [ "$left" -lt "$TIMEOUT_SECONDS" ]; then echo "$left"; else echo "$TIMEOUT_SECONDS"; fi
}

# poll_until WHAT COMMAND...: run COMMAND until it succeeds, to a deadline,
# checking the hard bound on every round. Bounded, the only sleep this repo
# sanctions (PRINCIPLES.md §5).
poll_until() {
  local what="$1" limit deadline
  shift
  checkpoint "$what"
  limit="$(step_timeout)"
  deadline=$((SECONDS + limit))
  until "$@" >/dev/null 2>&1; do
    checkpoint "$what"
    [ "$SECONDS" -lt "$deadline" ] || fail "${what}: not reached within ${limit}s ($(elapsed))"
    sleep "$POLL_SECONDS"
  done
}

# poll_soft SECONDS COMMAND...: poll_until for the teardown, which neither
# stops at the hard bound nor exits: it returns 1 at its deadline.
poll_soft() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep "$POLL_SECONDS"
  done
}

# --- traps: teardown first ------------------------------------------------------

# The watchdog: a background loop that sends TERM to this script once the
# clock passes T+3h. Bash runs the TERM trap as soon as the command in hand
# returns, and every wait is capped at the bound (step_timeout), so that is
# within one poll of T+3h. Killed before any teardown starts.
start_watchdog() {
  local main_pid=$$
  (
    trap - INT TERM ERR
    while kill -0 "$main_pid" 2>/dev/null; do
      if [ "$(now)" -ge "$HARD_DEADLINE" ]; then
        kill -TERM "$main_pid" 2>/dev/null || true
        exit 0
      fi
      sleep "$WATCHDOG_POLL_SECONDS"
    done
  ) </dev/null >/dev/null 2>&1 &
  watchdog_pid=$!
}

stop_watchdog() {
  [ -n "$watchdog_pid" ] || return 0
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  watchdog_pid=""
}

stop_port_forwards() {
  local pid
  # The ${...+...} form expands an empty array to nothing under `set -u` on
  # every bash, macOS's 3.2 included (smoke.sh's idiom).
  for pid in ${port_forward_pids[@]+"${port_forward_pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  port_forward_pids=()
}

on_signal() {
  if [ -n "$HARD_DEADLINE" ] && [ "$(now)" -ge "$HARD_DEADLINE" ]; then
    echo "fail: T+3h hard bound reached ($(elapsed)): the watchdog stopped the demo; teardown first (D39)" >&2
  elif [ "$create_started" -eq 1 ]; then
    echo "fail: interrupted by ${1} ($(elapsed)); teardown first (D39)" >&2
  else
    echo "fail: interrupted by ${1}" >&2
  fi
  case "$1" in
    INT) exit 130 ;;
    *) exit 143 ;;
  esac
}

# The EXIT trap. errexit routes every failure here — a fail line, a command
# that failed under `set -e` (the ERR trap recorded its line), or a signal
# above — and once cluster create has started, nothing leaves this script
# without a teardown.
on_exit() {
  local status=$?
  set +e
  trap - EXIT ERR
  trap '' INT TERM
  stop_port_forwards
  if [ "$status" -ne 0 ] && [ -n "$last_err_line" ]; then
    echo "fail: a command failed at deploy/eks/demo.sh line ${last_err_line} (exit ${status})" >&2
  fi
  if [ "$create_started" -eq 1 ] && [ "$torn_down" -eq 0 ]; then
    [ "$status" -ne 0 ] || status=1
    teardown failed
    echo "fail: the run did not complete; teardown-first ran: ${teardown_failures} teardown failure(s), ${sweep_failures} sweep line(s) not absent" >&2
  elif [ "$MODE" = run ] && [ "$create_started" -eq 0 ] && [ "$status" -ne 0 ]; then
    echo "note: nothing was created: the run stopped before eksctl create cluster, so there is nothing to tear down"
  fi
  stop_watchdog
  [ -z "$SCRATCH" ] || rm -rf "$SCRATCH"
  exit "$status"
}

trap 'last_err_line=$LINENO' ERR
trap on_exit EXIT
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

# --- preconditions --------------------------------------------------------------

require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || die "${tool} not found on PATH"
  done
}

require_cluster_name() {
  [[ "$CLUSTER_NAME" =~ $CLUSTER_NAME_PATTERN ]] \
    || die "the cluster name must be mlobs-demo-<8 hex>, the name a run printed; got '${CLUSTER_NAME}'"
}

# The profile is the identity's name (D39). Credentials in the environment
# outrank AWS_PROFILE in the AWS CLI and SDKs, eksctl's included, so they
# would quietly replace the designated principal: refused.
require_profile() {
  local var
  [ -n "${AWS_PROFILE:-}" ] \
    || die "AWS_PROFILE is not set: the demo runs only as the designated non-root admin principal, named by its profile (D39). Nothing was called."
  for var in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN; do
    [ -z "${!var:-}" ] \
      || die "${var} is set and would outrank AWS_PROFILE=${AWS_PROFILE}; unset it. Nothing was called."
  done
  export AWS_PROFILE
}

# The transcript's first line (D39).
identity() {
  local out arn account
  out="$(aws sts get-caller-identity --region "$REGION" --query '[Arn,Account]' --output text 2>/dev/null)" \
    || die "aws sts get-caller-identity failed for AWS_PROFILE=${AWS_PROFILE} (an SSO profile needs: aws sso login --profile ${AWS_PROFILE}). Nothing was created."
  read -r arn account <<<"$out" || true
  [ -n "${arn:-}" ] || die "aws sts get-caller-identity returned no ARN. Nothing was created."
  echo "identity: aws sts get-caller-identity: ${arn} (account ${account:-unknown}, AWS_PROFILE=${AWS_PROFILE})"
  case "$arn" in
    *:root*) die "the caller is the account root (${arn}); D39 rules root out: EKS binds cluster-creator admin to the creating principal for good. Run as the designated non-root admin principal. Nothing was created." ;;
  esac
  echo "ok: identity is not the account root"
}

test_seam_note() {
  local seams=""
  [ -z "${DEMO_TEST_CLOCK_FILE:-}" ] || seams="${seams} DEMO_TEST_CLOCK_FILE"
  [ -z "${DEMO_TEST_TOOLS_DIR:-}" ] || seams="${seams} DEMO_TEST_TOOLS_DIR"
  [ -z "$seams" ] || echo "note: test seams set:${seams}; this transcript is a harness run, not evidence"
}

# A scratch directory named for the cluster, so that `teardown` can find a
# dead run's. Everything the run writes goes here, its kubeconfig included,
# and it is removed on any exit.
make_scratch() {
  SCRATCH="$(mktemp -d "${TMP_ROOT}/${CLUSTER_NAME}.XXXXXX")"
  KUBECONFIG_PATH="${SCRATCH}/kubeconfig"
  # Every kubectl, helm and eksctl call reads and writes this file and no
  # other: ~/.kube/config is never touched (D39).
  export KUBECONFIG="$KUBECONFIG_PATH"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# fetch URL FILE: the pinned-download fetch. --speed-limit/--speed-time abort
# a transfer stalled under 1 KB/s for 30 s, and --max-time bounds the whole.
fetch() {
  curl -fsSL --retry 2 --connect-timeout 15 --speed-limit 1024 --speed-time 30 \
    --max-time 600 -o "$2" "$1"
}

# fetch_verified URL FILE SHA256 NAME: fetch, then refuse anything but the
# pinned bytes.
fetch_verified() {
  local got
  fetch "$1" "$2" || fail "pinned download failed: $1. Nothing was created."
  got="$(sha256_of "$2")"
  [ "$got" = "$3" ] \
    || fail "pinned download: $4 has sha256 ${got}, not the pinned $3. Nothing was created."
}

install_pinned_tools() {
  local os arch helm_platform eksctl_platform helm_sum eksctl_sum bin helm_from eksctl_from version
  if [ -n "${DEMO_TEST_TOOLS_DIR:-}" ]; then
    bin="$DEMO_TEST_TOOLS_DIR"
    helm_from="from DEMO_TEST_TOOLS_DIR, not downloaded"
    eksctl_from="$helm_from"
  else
    case "$(uname -s)" in
      Darwin) os=darwin ;;
      Linux) os=linux ;;
      *) die "unsupported OS $(uname -s): the pins cover Darwin and Linux" ;;
    esac
    case "$(uname -m)" in
      x86_64 | amd64) arch=amd64 ;;
      arm64 | aarch64) arch=arm64 ;;
      *) die "unsupported architecture $(uname -m): the pins cover amd64 and arm64" ;;
    esac
    helm_platform="${os}-${arch}"
    eksctl_platform="$(printf '%s' "${os:0:1}" | tr '[:lower:]' '[:upper:]')${os:1}_${arch}"
    helm_sum="$(helm_sha256 "$helm_platform")" || die "no pinned helm checksum for ${helm_platform}"
    eksctl_sum="$(eksctl_sha256 "$eksctl_platform")" || die "no pinned eksctl checksum for ${eksctl_platform}"
    bin="${SCRATCH}/bin"
    mkdir -p "$bin"

    fetch_verified "https://get.helm.sh/helm-${HELM_VERSION}-${helm_platform}.tar.gz" \
      "${SCRATCH}/helm.tar.gz" "$helm_sum" "helm-${HELM_VERSION}-${helm_platform}.tar.gz"
    tar -xzf "${SCRATCH}/helm.tar.gz" -C "$SCRATCH" "${helm_platform}/helm"
    mv "${SCRATCH}/${helm_platform}/helm" "${bin}/helm"
    chmod 0755 "${bin}/helm"
    helm_from="helm-${HELM_VERSION}-${helm_platform}.tar.gz, sha256 ${helm_sum} verified"

    fetch_verified "https://github.com/eksctl-io/eksctl/releases/download/v${EKSCTL_VERSION}/eksctl_${eksctl_platform}.tar.gz" \
      "${SCRATCH}/eksctl.tar.gz" "$eksctl_sum" "eksctl_${eksctl_platform}.tar.gz"
    tar -xzf "${SCRATCH}/eksctl.tar.gz" -C "$bin" eksctl
    chmod 0755 "${bin}/eksctl"
    eksctl_from="eksctl_${eksctl_platform}.tar.gz v${EKSCTL_VERSION}, sha256 ${eksctl_sum} verified"
  fi
  HELM="${bin}/helm"
  EKSCTL="${bin}/eksctl"

  version="$("$HELM" version --template '{{.Version}}' 2>/dev/null)" || version=""
  [ "$version" = "$HELM_VERSION" ] \
    || fail "helm reports '${version}', not the pinned ${HELM_VERSION} (D38). Nothing was created."
  echo "ok: helm ${version}, pinned (D38): ${helm_from}"
  version="$("$EKSCTL" version 2>/dev/null)" || version=""
  [ "$version" = "$EKSCTL_VERSION" ] \
    || fail "eksctl reports '${version}', not the pinned ${EKSCTL_VERSION} (D39). Nothing was created."
  echo "ok: eksctl ${version}, pinned (D39): ${eksctl_from}"
}

# kubectl is the owner's, not pinned; Kubernetes supports it within one minor
# version of the API server, so anything outside 1.35-1.37 is refused here.
check_kubectl() {
  local client minor
  client="$(kubectl version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion // empty')" || client=""
  minor="$(printf '%s' "$client" | sed -n 's/^v1\.\([0-9][0-9]*\)\..*/\1/p')"
  [ -n "$minor" ] && [ "$minor" -ge 35 ] && [ "$minor" -le 37 ] \
    || fail "kubectl client '${client}' is outside one minor version of the ${EKS_VERSION} API server (Kubernetes' version-skew policy). Nothing was created."
  echo "ok: kubectl client ${client}, within one minor version of ${EKS_VERSION}"
}

# --- same-day verification (D39) ------------------------------------------------

# EKS's own answer for today, without a cluster: DescribeClusterVersions.
verify_eks_version() {
  local out version status until patch
  out="$(aws eks describe-cluster-versions --region "$REGION" --cluster-versions "$EKS_VERSION" \
      --query 'clusterVersions[0].[clusterVersion,versionStatus,endOfStandardSupportDate,kubernetesPatchVersion]' \
      --output text 2>/dev/null)" \
    || fail "aws eks describe-cluster-versions failed in ${REGION}. Nothing was created."
  read -r version status until patch <<<"$out" || true
  [ "${version:-}" = "$EKS_VERSION" ] \
    || fail "EKS ${EKS_VERSION} is not offered in ${REGION} today (describe-cluster-versions returned '${out}'). Nothing was created."
  [ "${status:-}" = STANDARD_SUPPORT ] \
    || fail "EKS ${EKS_VERSION} in ${REGION} is ${status:-unknown} today, not STANDARD_SUPPORT: D39's band and the \$0.10/h rate both assume standard support. Nothing was created."
  echo "ok: EKS ${version} in ${REGION}: STANDARD_SUPPORT until ${until:-unknown}, patch ${patch:-unknown} (aws eks describe-cluster-versions, read $(utc_of "$(now)"))"
}

# The rate from AWS's public price list, read today: one on-demand rate at the
# standard per-cluster usage type, equal to D39's $0.10/h.
verify_eks_rate() {
  local file="${SCRATCH}/eks-price-list.json" rates published
  fetch "$PRICE_LIST_URL" "$file" \
    || fail "could not read the AWS Price List at ${PRICE_LIST_URL}. Nothing was created."
  rates="$(jq -r --arg ut "$EKS_USAGE_TYPE" '
      [.products[] | select(.attributes.usagetype == $ut) | .sku] as $skus
      | [$skus[] as $s | (.terms.OnDemand[$s] // {})[] | .priceDimensions[] | .pricePerUnit.USD]
      | unique | join(" ")' "$file" 2>/dev/null)" || rates=""
  published="$(jq -r '.publicationDate // "unknown"' "$file" 2>/dev/null)" || published=unknown
  case "$rates" in
    "" | *" "*) fail "the AWS Price List shows '${rates}' for ${EKS_USAGE_TYPE}, not one rate. Nothing was created." ;;
  esac
  awk -v r="$rates" -v want="$EKS_RATE_USD" 'BEGIN { exit !(r + 0 == want + 0) }' \
    || fail "the EKS standard-support control plane is \$${rates}/h in ${REGION} today, not the \$${EKS_RATE_USD}/h of D39's cost arithmetic; re-rule the arithmetic before creating a cluster. Nothing was created."
  echo "ok: EKS control plane, standard support: \$${EKS_RATE_USD}/h in ${REGION} (${EKS_USAGE_TYPE} at ${rates} USD/h; source: the AWS Price List, ${PRICE_LIST_URL}, publicationDate ${published}; read $(utc_of "$(now)"))"
}

# --- the images (D18, D39) -------------------------------------------------------

verify_on_main() {
  local sha="$1" tip="$2" status=0
  git -C "$REPO_ROOT" merge-base --is-ancestor "$sha" origin/main 2>/dev/null || status=$?
  case "$status" in
    0) echo "ok: ${sha} is on main (local origin/main at ${tip})" ;;
    1) fail "${sha} is not an ancestor of origin/main: the demo runs real main SHAs only (D39). Nothing was created." ;;
    *) fail "${sha} is not a commit in this repository (git exited ${status}; git fetch origin first). Nothing was created." ;;
  esac
}

# ghcr_check IMAGE SHA: deploy.yml's preflight — the anonymous registry token,
# then a HEAD on the manifest, whose plain status separates "no such tag"
# (404) from every other failure. Prints its line; returns 1 on a miss.
ghcr_check() {
  local repo="${GHCR_OWNER}/$1" sha="$2" token code
  token="$(curl -fsS --connect-timeout 15 --max-time 60 \
      "https://ghcr.io/token?scope=repository:${repo}:pull" | jq -r '.token // empty')" || token=""
  if [ -z "$token" ]; then
    echo "fail: preflight: could not get an anonymous pull token for ghcr.io/${repo}" >&2
    return 1
  fi
  code="$(curl -sS --connect-timeout 15 --max-time 60 -o /dev/null -w '%{http_code}' -I \
      -H "Authorization: Bearer ${token}" -H "Accept: ${GHCR_ACCEPT}" \
      "https://ghcr.io/v2/${repo}/manifests/${sha}")" || code="request-failed"
  case "$code" in
    200)
      echo "ok: preflight: ghcr.io/${repo}:${sha} (anonymous-token manifest check)"
      ;;
    404)
      echo "fail: preflight: ghcr.io/${repo}:${sha} not found — GhcrPublish pushes it on the main push for that commit; check that run" >&2
      return 1
      ;;
    *)
      echo "fail: preflight: ghcr.io/${repo}:${sha} check returned ${code}" >&2
      return 1
      ;;
  esac
}

# The run's three GHCR pulls, all checked before failing on any: the operator
# at the stable SHA (the chart's image.tag), and the api at both SHAs. redis is
# the pinned public library image 11-redis.yaml already names.
preflight_images() {
  local missing=0
  ghcr_check mlobs-operator "$STABLE_SHA" || missing=1
  ghcr_check mlobs-api "$STABLE_SHA" || missing=1
  ghcr_check mlobs-api "$CANARY_SHA" || missing=1
  [ "$missing" -eq 0 ] || fail "preflight: an image the run pulls is missing from GHCR. Nothing was created."
}

# --- the cluster ------------------------------------------------------------------

new_cluster_name() {
  CLUSTER_NAME="mlobs-demo-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
  require_cluster_name
}

# As apply.sh renders a manifest: sed the placeholder, then refuse any that
# survives.
render_cluster_config() {
  sed -e "s|CLUSTER_NAME|${CLUSTER_NAME}|g" "$CLUSTER_CONFIG" > "${SCRATCH}/cluster.yaml"
  if grep -q 'CLUSTER_NAME' "${SCRATCH}/cluster.yaml"; then
    fail "unsubstituted placeholder left in the rendered cluster config. Nothing was created."
  fi
  grep -qE "^  name: ${CLUSTER_NAME}\$" "${SCRATCH}/cluster.yaml" \
    || fail "the rendered cluster config does not name ${CLUSTER_NAME}. Nothing was created."
  echo "ok: deploy/eks/cluster.yaml rendered for ${CLUSTER_NAME}"
}

create_cluster() {
  T0="$(now)"
  VALVE_AT=$((T0 + VALVE_SECONDS))
  HARD_DEADLINE=$((T0 + HARD_BOUND_SECONDS))
  create_started=1
  start_watchdog
  echo "ok: teardown-first armed: from here a failure, an interrupt or the T+3h bound tears ${CLUSTER_NAME} down first (EXIT/ERR/INT/TERM traps, a watchdog on the bound)"
  echo "ok: T0 $(utc_of "$T0"): eksctl create cluster starts; the T+2h window-segment valve at $(utc_of "$VALVE_AT"), the hard T+3h bound at $(utc_of "$HARD_DEADLINE") (D39)"
  if ! "$EKSCTL" create cluster --config-file "${SCRATCH}/cluster.yaml" \
      --kubeconfig "$KUBECONFIG_PATH" --timeout "$EKSCTL_TIMEOUT" 2>&1 | indent; then
    fail "eksctl create cluster ${CLUSTER_NAME} failed ($(elapsed))"
  fi
  [ -s "$KUBECONFIG_PATH" ] || fail "eksctl create cluster wrote no kubeconfig at ${KUBECONFIG_PATH}"
  echo "ok: eksctl create cluster: ${CLUSTER_NAME} created ($(elapsed)); kubeconfig ${KUBECONFIG_PATH}, a scratch file, never ~/.kube/config"
}

# --- the demonstration (D39) ------------------------------------------------------

k() {
  kubectl --namespace "$NAMESPACE" "$@"
}

field() {
  k get "$1" -o "jsonpath=$2" 2>/dev/null
}

assert_node() {
  local rows count name ready type ip
  checkpoint "the node Ready"
  kubectl wait --for=condition=Ready nodes --all --timeout="$(step_timeout)s" >/dev/null 2>&1 \
    || fail "the node is not Ready ($(elapsed))"
  kubectl get nodes -o wide 2>&1 | indent
  rows="$(kubectl get nodes -o 'jsonpath={range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status} {.metadata.labels.node\.kubernetes\.io/instance-type} {.status.addresses[?(@.type=="ExternalIP")].address}{"\n"}{end}')" \
    || fail "could not read the nodes"
  count="$(printf '%s\n' "$rows" | grep -c . || true)"
  [ "$count" -eq 1 ] || fail "expected exactly one node, found ${count}"
  read -r name ready type ip <<<"$rows" || true
  [ "${ready:-}" = True ] || fail "node ${name} is not Ready"
  [ "${type:-}" = t3.medium ] || fail "node ${name} is a '${type:-}', not the t3.medium of deploy/eks/cluster.yaml"
  [ -n "${ip:-}" ] || fail "node ${name} has no public IPv4 (ExternalIP) address; privateNetworking false should give it one (D39)"
  echo "ok: node ${name} Ready: ${type}, public IPv4 ${ip} (public subnet, D39)"
}

assert_server_version() {
  local server
  kubectl version 2>&1 | indent
  server="$(kubectl version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // empty')" || server=""
  case "$server" in
    "v${EKS_VERSION}."*) ;;
    *) fail "the API server reports '${server}', not Kubernetes ${EKS_VERSION}" ;;
  esac
  echo "ok: kubectl version: server ${server}, Kubernetes ${EKS_VERSION} (D39)"
}

# Reuse, not copies (D39): the two files as they sit in deploy/k3s/manifests,
# with no render — neither carries a placeholder.
apply_namespace_and_redis() {
  local manifest
  checkpoint "the namespace and redis"
  for manifest in 00-namespace.yaml 11-redis.yaml; do
    kubectl apply -f "${MANIFESTS}/${manifest}" 2>&1 | indent \
      || fail "kubectl apply -f deploy/k3s/manifests/${manifest} failed"
  done
  k rollout status deployment/redis --timeout="$(step_timeout)s" 2>&1 | indent \
    || fail "rollout did not converge: deployment/redis ($(elapsed))"
  echo "ok: deploy/k3s/manifests/00-namespace.yaml and 11-redis.yaml applied verbatim (kubectl apply -f, no render); deployment/redis rolled out"
}

install_chart() {
  local limit
  checkpoint "helm install"
  limit="$(step_timeout)"
  "$HELM" install "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
      --set image.prefix="$IMAGE_PREFIX" --set image.tag="$STABLE_SHA" \
      --wait --timeout "${limit}s" 2>&1 | indent \
    || fail "helm install ${RELEASE} -n ${NAMESPACE} failed ($(elapsed))"
  kubectl wait --for=condition=Established "crd/${CRD_NAME}" --timeout="$(step_timeout)s" >/dev/null 2>&1 \
    || fail "crd/${CRD_NAME} not Established ($(elapsed))"
  echo "ok: helm install ${RELEASE} -n ${NAMESPACE}: deploy/helm/mlobs-operator at image.prefix ${IMAGE_PREFIX}, image.tag ${STABLE_SHA}; crd/${CRD_NAME} Established from the chart's crds/"
}

lease_name=""
operator_pod=""

# A holder identity is the holder's hostname, its pod name, then _ and a UUID.
lease_held() {
  local holder
  holder="$(field "lease/${lease_name}" '{.spec.holderIdentity}')" || return 1
  [ -n "$operator_pod" ] && [ "${holder%%_*}" = "$operator_pod" ]
}

assert_operator() {
  local image
  checkpoint "the operator Ready"
  # e2e.sh's read: the Lease's name is the operator's own literal.
  lease_name="$(sed -n 's/^[[:space:]]*leaderElectionID = "\(.*\)"$/\1/p' "${REPO_ROOT}/operator/cmd/main.go")"
  [ -n "$lease_name" ] || fail "leaderElectionID not found in operator/cmd/main.go"
  k rollout status deployment/operator --timeout="$(step_timeout)s" >/dev/null 2>&1 \
    || fail "rollout did not converge: deployment/operator ($(elapsed))"
  operator_pod="$(k get pods -l app=operator -o 'jsonpath={.items[0].metadata.name}' 2>/dev/null)" || operator_pod=""
  [ -n "$operator_pod" ] || fail "no operator pod found"
  poll_until "lease/${lease_name} held by pod/${operator_pod}" lease_held
  image="$(field deployment/operator '{.spec.template.spec.containers[?(@.name=="manager")].image}')" || image=""
  [ "$image" = "${IMAGE_PREFIX}/mlobs-operator:${STABLE_SHA}" ] \
    || fail "deployment/operator runs '${image}', not the chart's ${IMAGE_PREFIX}/mlobs-operator:${STABLE_SHA}"
  k get deployment/operator "lease/${lease_name}" -o wide 2>&1 | indent
  echo "ok: operator Ready: deployment/operator at ${image}; lease/${lease_name} held by pod/${operator_pod} (D35)"
}

# condition_at_generation TYPE STATUS: the ServingDeployment's TYPE condition
# reads STATUS, computed for its current generation — status.observedGeneration
# and the condition's own both equal metadata.generation (apply.sh's test).
condition_at_generation() {
  local generation observed status condition_generation
  read -r generation observed status condition_generation <<<"$(field "$SERVINGDEPLOYMENT" \
    "{.metadata.generation} {.status.observedGeneration} {.status.conditions[?(@.type==\"$1\")].status} {.status.conditions[?(@.type==\"$1\")].observedGeneration}")" \
    || return 1
  [ -n "$generation" ] && [ "$generation" = "$observed" ] \
    && [ "$status" = "$2" ] && [ "$condition_generation" = "$generation" ] || return 1
  sd_generation="$generation"
}

apply_servingdeployment() {
  local rendered="${SCRATCH}/40-servingdeployment.yaml" owner image
  checkpoint "the ServingDeployment"
  # apply.sh's render, to the letter: the image prefix and tag, then any
  # surviving placeholder refused.
  sed -e "s|IMAGE_PREFIX|${IMAGE_PREFIX}|g" \
      -e "s|IMAGE_TAG|${STABLE_SHA}|g" \
      "${MANIFESTS}/40-servingdeployment.yaml" > "$rendered"
  if grep -qE 'IMAGE_PREFIX|IMAGE_TAG|PLACEHOLDER' "$rendered"; then
    fail "unsubstituted placeholder left in the rendered 40-servingdeployment.yaml"
  fi
  kubectl apply -f "$rendered" 2>&1 | indent || fail "kubectl apply of the rendered 40-servingdeployment.yaml failed"
  poll_until "${SERVINGDEPLOYMENT} Ready at its current generation" condition_at_generation Ready True
  owner="$(field deployment/api \
    '{.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}')" || owner=""
  [ "$owner" = "ServingDeployment/api" ] || fail "deployment/api is controlled by '${owner}', not ServingDeployment/api"
  image="$(field deployment/api '{.spec.template.spec.containers[?(@.name=="api")].image}')" || image=""
  [ "$image" = "${IMAGE_PREFIX}/mlobs-api:${STABLE_SHA}" ] || fail "deployment/api runs '${image}', not the rendered tag"
  k get "$SERVINGDEPLOYMENT" deployment/api -o wide 2>&1 | indent
  echo "ok: deployment/api created and controlled by ${SERVINGDEPLOYMENT} at ${image}; Ready at observedGeneration == generation (${sd_generation})"
}

# port_forward RESOURCE LOCAL_PORT READY_PATH: smoke.sh's, to a deadline.
port_forward() {
  local resource="$1" local_port="$2" ready_path="$3" pid deadline
  kubectl port-forward "$resource" "${local_port}:8000" --namespace "$NAMESPACE" >/dev/null 2>&1 &
  pid=$!
  port_forward_pids+=("$pid")
  deadline=$((SECONDS + 60))
  until curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:${local_port}${ready_path}" 2>/dev/null; do
    kill -0 "$pid" 2>/dev/null || fail "kubectl port-forward to ${resource} exited"
    [ "$SECONDS" -lt "$deadline" ] || fail "${resource} not reachable over a port-forward within 60s"
    sleep "$POLL_SECONDS"
  done
}

# Each curl below is its own process, so each request is a fresh connection
# through the forward: nothing is kept alive between them.
check_health() {
  local port="$1" what="$2" body="${SCRATCH}/health.json" code
  code="$(curl -sS --max-time 15 -o "$body" -w '%{http_code}' "http://127.0.0.1:${port}/health")" \
    || code="request-failed"
  show_file "$body"
  [ "$code" = 200 ] || fail "${what}: GET /health returned ${code}, expected 200"
  echo "ok: ${what} /health 200 through a port-forward"
}

check_predict() {
  local port="$1" what="$2" body="${SCRATCH}/predict.json" code
  code="$(curl -sS --max-time 30 -o "$body" -w '%{http_code}' -X POST \
      -H 'Content-Type: application/json' -d '{"text":"eks demonstration sentence"}' \
      "http://127.0.0.1:${port}/predict")" || code="request-failed"
  show_file "$body"
  [ "$code" = 200 ] || fail "${what}: POST /predict returned ${code}, expected 200"
  jq -e 'has("request_id") and has("label") and has("confidence")' "$body" >/dev/null 2>&1 \
    || fail "${what}: POST /predict response lacks one of request_id, label, confidence"
  echo "ok: ${what} /predict 200 with request_id, label, confidence through a port-forward"
}

serve_stable() {
  checkpoint "stable serving"
  port_forward deployment/api "$STABLE_LOCAL_PORT" /health
  check_health "$STABLE_LOCAL_PORT" deployment/api
  check_predict "$STABLE_LOCAL_PORT" deployment/api
  stop_port_forwards
}

# The pre-decided de-scope line (D39, owner ruling 2026-10-05).
window_valve() {
  checkpoint "the window segment"
  if [ "$(now)" -ge "$VALVE_AT" ]; then
    valve_used=1
    echo "valve: T+2h window-segment cut at $(elapsed) (D39): the window segment is skipped; the demo stands at chart install, CR Ready at generation and stable serving, and goes to teardown and sweep"
  else
    echo "ok: valve not used: $(elapsed) at the window segment, under T+2h (D39)"
  fi
}

open_window() {
  local image owner
  checkpoint "the window open"
  k patch "$SERVINGDEPLOYMENT" --type merge \
      -p "{\"spec\":{\"canaryImageTag\":\"${CANARY_SHA}\",\"canaryReplicas\":1}}" 2>&1 | indent \
    || fail "the canary patch on ${SERVINGDEPLOYMENT} failed"
  poll_until "CanaryActive True at the current generation" condition_at_generation CanaryActive True
  poll_until "deployment/api-canary created" k get deployment/api-canary
  k rollout status deployment/api-canary --timeout="$(step_timeout)s" >/dev/null 2>&1 \
    || fail "rollout did not converge: deployment/api-canary ($(elapsed))"
  image="$(field deployment/api-canary '{.spec.template.spec.containers[?(@.name=="api")].image}')" || image=""
  [ "$image" = "${IMAGE_PREFIX}/mlobs-api:${CANARY_SHA}" ] || fail "deployment/api-canary runs '${image}', not the canary tag"
  owner="$(field deployment/api-canary \
    '{.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}')" || owner=""
  [ "$owner" = "ServingDeployment/api" ] || fail "deployment/api-canary is controlled by '${owner}', not ServingDeployment/api"
  k get "$SERVINGDEPLOYMENT" deployment/api deployment/api-canary -o wide 2>&1 | indent
  echo "ok: window open ($(elapsed)): CanaryActive True at generation ${sd_generation}; deployment/api-canary at ${image}, controlled by ${SERVINGDEPLOYMENT}"
}

# The canary's own counter, read off its own pod through the forward.
canary_predictions() {
  curl -fsS --max-time 15 "http://127.0.0.1:${CANARY_LOCAL_PORT}/metrics" 2>/dev/null \
    | awk '/^mlobs_predictions_total[{ ]/ { total += $NF } END { printf "%d\n", total }'
}

# What "canary observed serving" means on EKS, exactly (D39): pod-scoped.
canary_serving() {
  local before after
  checkpoint "the canary serving"
  port_forward deployment/api-canary "$CANARY_LOCAL_PORT" /metrics
  before="$(canary_predictions)" || fail "could not read the canary's mlobs_predictions_total"
  check_predict "$CANARY_LOCAL_PORT" deployment/api-canary
  after="$(canary_predictions)" || fail "could not read the canary's mlobs_predictions_total"
  stop_port_forwards
  [ "$after" -gt "$before" ] \
    || fail "the canary's own mlobs_predictions_total did not increase across the forward (${before} -> ${after})"
  echo "ok: canary serving, pod-scoped (D39): the canary's own mlobs_predictions_total ${before} -> ${after} across the port-forward to deployment/api-canary"
  echo "note: stated, not demonstrated here: the D34 per-connection split and the NodePort are k3s properties, proven by K3sSmoke and the live host; no svc/api or svc/api-canary exists on EKS (D39)"
}

shadow_as_expected() {
  local shadow
  checkpoint "ShadowPaused"
  shadow="$(field "$SERVINGDEPLOYMENT" \
    '{.status.conditions[?(@.type=="ShadowPaused")].status} {.status.conditions[?(@.type=="ShadowPaused")].reason}')" || shadow=""
  [ "$shadow" = "False ShadowNotFound" ] \
    || fail "ShadowPaused reads '${shadow}', expected False with reason ShadowNotFound: no shadow StatefulSet exists on EKS (D39)"
  echo "ok: ShadowPaused False, reason ShadowNotFound: expected, no shadow StatefulSet exists on EKS, so there is nothing to pause (D39)"
}

canary_gone() {
  local replicas phases
  replicas="$(k get deployment/api-canary --ignore-not-found -o 'jsonpath={.spec.replicas}')" || return 1
  [ -z "$replicas" ] || [ "$replicas" = 0 ] || return 1
  phases="$(k get pods -l app=api,role=canary -o 'jsonpath={range .items[*]}{.status.phase}{"\n"}{end}')" || return 1
  ! printf '%s\n' "$phases" | grep -qE '^(Pending|Running|Unknown)$'
}

close_window() {
  local close_patch
  checkpoint "the window close"
  # apply.sh's constant, read from apply.sh rather than copied (e2e.sh's read).
  close_patch="$(sed -n "s/^CLOSE_WINDOW_PATCH='\(.*\)'\$/\1/p" "${REPO_ROOT}/deploy/k3s/apply.sh")"
  [ -n "$close_patch" ] || fail "CLOSE_WINDOW_PATCH not found in deploy/k3s/apply.sh"
  k patch "$SERVINGDEPLOYMENT" --type merge -p "$close_patch" 2>&1 | indent \
    || fail "the close patch on ${SERVINGDEPLOYMENT} failed"
  echo "ok: the constant close patch sent: ${close_patch} (apply.sh's CLOSE_WINDOW_PATCH, read from it verbatim)"
  poll_until "CanaryActive False at the current generation" condition_at_generation CanaryActive False
  poll_until "Ready True at the current generation" condition_at_generation Ready True
  poll_until "deployment/api-canary at 0 with no pods" canary_gone
  k get "$SERVINGDEPLOYMENT" deployment/api deployment/api-canary --ignore-not-found -o wide 2>&1 | indent
  port_forward deployment/api "$STABLE_LOCAL_PORT" /health
  check_health "$STABLE_LOCAL_PORT" deployment/api
  stop_port_forwards
  echo "ok: steady ($(elapsed)): CanaryActive False and Ready True at generation ${sd_generation}; deployment/api-canary at 0 with no pods; deployment/api serving"
}

# --- teardown (D36-shaped, D39) ---------------------------------------------------

teardown_fail() {
  echo "fail: teardown: $1" >&2
  teardown_failures=$((teardown_failures + 1))
}

api_collected() {
  [ -z "$(k get deployment/api deployment/api-canary --ignore-not-found -o name 2>/dev/null)" ]
}

# The D36 order on a healthy cluster: the ServingDeployment first, while the
# operator still runs; what it owns follows through the garbage collector;
# then the release; then the CRD, which Helm's install-only crds/ never
# removes (D38) and which deleting earlier would take the ServingDeployment
# with it. Each step is reported and the next one runs regardless: nothing
# here may stand between the cluster and its delete.
in_cluster_teardown() {
  local t="$TEARDOWN_TIMEOUT_SECONDS"
  if k delete "$SERVINGDEPLOYMENT" --timeout="${t}s" 2>&1 | indent; then
    echo "ok: teardown: ${SERVINGDEPLOYMENT} deleted"
  else
    teardown_fail "kubectl delete ${SERVINGDEPLOYMENT} failed"
  fi
  if poll_soft "$t" api_collected; then
    echo "ok: teardown: deployment/api and deployment/api-canary garbage-collected through their ownerReferences to ${SERVINGDEPLOYMENT}"
  else
    teardown_fail "deployment/api or deployment/api-canary still present ${t}s after their owner's delete"
  fi
  if "$HELM" uninstall "$RELEASE" --namespace "$NAMESPACE" --wait --timeout "${t}s" 2>&1 | indent; then
    echo "ok: teardown: helm uninstall ${RELEASE} -n ${NAMESPACE}"
  else
    teardown_fail "helm uninstall ${RELEASE} -n ${NAMESPACE} failed"
  fi
  if kubectl get crd "$CRD_NAME" >/dev/null 2>&1; then
    echo "ok: teardown: crd/${CRD_NAME} still present after helm uninstall: crds/ is install-only (D38)"
  else
    teardown_fail "crd/${CRD_NAME} is gone after helm uninstall; crds/ should be install-only (D38)"
  fi
  if kubectl delete crd "$CRD_NAME" --timeout="${t}s" 2>&1 | indent; then
    echo "ok: teardown: crd/${CRD_NAME} deleted explicitly, last"
  else
    teardown_fail "kubectl delete crd ${CRD_NAME} failed"
  fi
}

# eksctl deletes the cluster's stacks whether or not the EKS cluster itself
# still exists; when neither does it says so, and that is not a failure: the
# sweep is the arbiter of what is left.
delete_cluster() {
  local out="${SCRATCH}/eksctl-delete.log" status=0
  "$EKSCTL" delete cluster --name "$CLUSTER_NAME" --region "$REGION" --wait \
    --timeout "$EKSCTL_TIMEOUT" >"$out" 2>&1 || status=$?
  indent <"$out"
  DELETED_AT="$(now)"
  if [ "$status" -eq 0 ]; then
    echo "ok: eksctl delete cluster --wait: ${CLUSTER_NAME} deleted ($(elapsed))"
  elif grep -q 'does not exist' "$out"; then
    echo "ok: eksctl delete cluster: neither a cluster nor a cluster stack named ${CLUSTER_NAME} exists; nothing for it to delete, and the sweep decides what is left"
  else
    teardown_fail "eksctl delete cluster ${CLUSTER_NAME} exited ${status}; re-run: AWS_PROFILE=${AWS_PROFILE} deploy/eks/demo.sh teardown ${CLUSTER_NAME}"
  fi
}

remove_kubeconfig() {
  rm -f "$KUBECONFIG_PATH"
  if [ -e "$KUBECONFIG_PATH" ]; then
    teardown_fail "the scratch kubeconfig ${KUBECONFIG_PATH} could not be deleted"
  else
    echo "ok: scratch kubeconfig deleted: ${KUBECONFIG_PATH}"
  fi
}

# A dead run's scratch directory, kubeconfig and all, found by its name.
remove_leftover_scratch() {
  local dir found=0
  for dir in "${TMP_ROOT}/${CLUSTER_NAME}".*; do
    [ -e "$dir" ] || continue
    [ "$dir" != "$SCRATCH" ] || continue
    rm -rf "$dir"
    found=1
    echo "ok: a leftover scratch directory of ${CLUSTER_NAME} removed, its kubeconfig with it: ${dir}"
  done
  [ "$found" -eq 1 ] || echo "ok: no leftover scratch kubeconfig of ${CLUSTER_NAME} under ${TMP_ROOT}"
}

# --- the orphan sweep (D39, pinned) -----------------------------------------------

# words TEXT: the AWS CLI's text output as one id per line, without the
# "None" it prints for a null. awk, not grep: BSD grep, macOS's, refuses an
# empty alternative such as 'None|' outright, and an id filter that errors
# must never read as "no ids".
words() {
  printf '%s\n' "$1" | tr -s ' \t' '\n' | awk 'NF && $0 != "None"'
}

# sweep_report LABEL FAILED FOUND [NOTE]: one fixed line. A query that failed
# proves nothing, so it fails the line rather than read as absent — and so
# does a failure to read the ids out of its answer.
sweep_report() {
  local label="$1" failed="$2" ids note="${4:-}"
  if ! ids="$(words "$3" | sort -u | tr '\n' ' ')"; then
    failed=1
    ids=""
  fi
  ids="${ids% }"
  sweep_lines=$((sweep_lines + 1))
  if [ "$failed" -ne 0 ]; then
    echo "fail: sweep: ${label}: a query failed, so absence is unproven" >&2
    sweep_failures=$((sweep_failures + 1))
  elif [ -n "$ids" ]; then
    echo "fail: sweep: ${label}: PRESENT: ${ids}" >&2
    sweep_failures=$((sweep_failures + 1))
  else
    echo "ok: sweep: ${label}: none${note:+ (${note})}"
  fi
}

# sweep_ec2 LABEL SUBCOMMAND QUERY [FILTER...]: one EC2 describe call per tag
# family — filters within one call are ANDed — with any FILTER added to each.
sweep_ec2() {
  local label="$1" sub="$2" query="$3" filter out found="" failed=0
  shift 3
  for filter in "${ec2_family_filters[@]}"; do
    if out="$(aws ec2 "$sub" --region "$REGION" --filters "$filter" ${@+"$@"} \
        --query "$query" --output text 2>/dev/null)"; then
      found="${found} ${out}"
    else
      failed=1
    fi
  done
  sweep_report "$label" "$failed" "$found"
}

# sweep_iam LABEL NOTE LIST_COMMAND LIST_QUERY TAGS_COMMAND ID_FLAG: IAM's
# list calls return no tags, so every id is listed and each one's tags are
# read with the family match. Only the ids whose tags match are reported.
sweep_iam() {
  local label="$1" note="$2" list_cmd="$3" list_query="$4" tags_cmd="$5" id_flag="$6"
  local ids id out found="" failed=0
  if ! ids="$(aws iam "$list_cmd" --query "$list_query" --output text 2>/dev/null)" \
      || ! ids="$(words "$ids")"; then
    sweep_report "$label" 1 "" "$note"
    return
  fi
  for id in $ids; do
    if out="$(aws iam "$tags_cmd" "$id_flag" "$id" --query "Tags[?${family_match}].Key" --output text 2>/dev/null)"; then
      case "$out" in "" | None) ;; *) found="${found} ${id}" ;; esac
    else
      failed=1
    fi
  done
  sweep_report "$label" "$failed" "$found" "$note"
}

# The sweep filters ONLY by the three cluster-scoped tag families eksctl and
# EKS stamp — alpha.eksctl.io/cluster-name, eks:cluster-name,
# kubernetes.io/cluster/<name> — and NEVER by a project tag: a project=mlobs
# sweep would enumerate the live P1 host (D39). The two by-construction lines
# look the cluster's own name up instead: its log group's path, and nothing
# else.
sweep() {
  local name="$1" filter out found failed
  local -a ec2_family_filters=(
    "Name=tag:alpha.eksctl.io/cluster-name,Values=${name}"
    "Name=tag:eks:cluster-name,Values=${name}"
    "Name=tag-key,Values=kubernetes.io/cluster/${name}"
  )
  local -a tagging_family_filters=(
    "Key=alpha.eksctl.io/cluster-name,Values=${name}"
    "Key=eks:cluster-name,Values=${name}"
    "Key=kubernetes.io/cluster/${name}"
  )
  local family_match="(Key=='alpha.eksctl.io/cluster-name' && Value=='${name}') || (Key=='eks:cluster-name' && Value=='${name}') || Key=='kubernetes.io/cluster/${name}'"
  local log_group="/aws/eks/${name}/cluster"
  sweep_lines=0
  sweep_failures=0

  echo "ok: sweep: ${name} in ${REGION}, by the three cluster-scoped tag families only: alpha.eksctl.io/cluster-name=${name}, eks:cluster-name=${name}, kubernetes.io/cluster/${name}; never a project tag (D39)"

  sweep_ec2 "EC2 instances (any state but terminated)" describe-instances \
    'Reservations[].Instances[].InstanceId' \
    "Name=instance-state-name,Values=pending,running,shutting-down,stopping,stopped"
  sweep_ec2 "security groups" describe-security-groups 'SecurityGroups[].GroupId'
  sweep_ec2 "network interfaces (ENIs, the DependencyViolation class)" describe-network-interfaces \
    'NetworkInterfaces[].NetworkInterfaceId'
  sweep_ec2 "launch templates" describe-launch-templates 'LaunchTemplates[].LaunchTemplateId'

  # describe-stacks lists every stack but DELETE_COMPLETE, so a stack stuck in
  # DELETE_FAILED, ROLLBACK_COMPLETE or any other terminal failure is listed
  # and reported with its status.
  failed=0
  out="$(aws cloudformation describe-stacks --region "$REGION" \
      --query "Stacks[?Tags[?${family_match}]].join(':', [StackName, StackStatus])" \
      --output text 2>/dev/null)" || failed=1
  sweep_report "CloudFormation stacks (every status but DELETE_COMPLETE, terminal failures included)" \
    "$failed" "${out:-}"

  sweep_ec2 "VPCs" describe-vpcs 'Vpcs[].VpcId'

  found=""
  failed=0
  for filter in "${tagging_family_filters[@]}"; do
    if out="$(aws resourcegroupstaggingapi get-resources --region "$REGION" \
        --resource-type-filters elasticloadbalancing:loadbalancer --tag-filters "$filter" \
        --query 'ResourceTagMappingList[].ResourceARN' --output text 2>/dev/null)"; then
      found="${found} ${out}"
    else
      failed=1
    fi
  done
  sweep_report "load balancers" "$failed" "$found" "none is created; asserted anyway"

  sweep_iam "IAM roles" "" list-roles 'Roles[].RoleName' list-role-tags --role-name

  failed=0
  out="$(aws logs describe-log-groups --region "$REGION" --log-group-name-prefix "$log_group" \
      --query "logGroups[?logGroupName=='${log_group}'].logGroupName" --output text 2>/dev/null)" || failed=1
  sweep_report "CloudWatch log group ${log_group}" "$failed" "${out:-}" \
    "expected-absent by construction: control-plane logging off"

  sweep_iam "IAM OIDC providers" "expected-absent by construction: withOIDC false" \
    list-open-id-connect-providers 'OpenIDConnectProviderList[].Arn' \
    list-open-id-connect-provider-tags --open-id-connect-provider-arn

  if [ "$sweep_failures" -eq 0 ]; then
    echo "ok: sweep: all-absent for ${name}: ${sweep_lines} lines (D39)"
  else
    echo "fail: sweep: ${sweep_failures} of ${sweep_lines} lines not absent for ${name}; delete what is listed (the CloudFormation console for a stack in DELETE_FAILED), then re-run: deploy/eks/demo.sh sweep ${name}" >&2
  fi
}

# --- the backstop, the cost line and the T+24h instruction (D39) ------------------

# infra/ec2 and its state untouched, asserted the way CI's TerraformPlan reads
# a plan: -detailed-exitcode, 0 for no changes, -lock=false, the output never
# printed (it carries the SSH ingress CIDR). Skipped, on a fixed line, where
# this machine cannot run it; the owner runs it by hand then.
terraform_backstop() {
  local command_line="terraform -chdir=infra/ec2 plan -input=false -lock=false -detailed-exitcode" code=0
  if ! command -v terraform >/dev/null 2>&1; then
    echo "skip: terraform -chdir=infra/ec2 plan: terraform is not on PATH; the owner runs it by hand and records exit 0 (D39): ${command_line}"
    return
  fi
  if [ -z "${TF_VAR_ssh_ingress_cidr:-}" ] && [ ! -f "${REPO_ROOT}/infra/ec2/terraform.tfvars" ]; then
    echo "skip: terraform -chdir=infra/ec2 plan: neither TF_VAR_ssh_ingress_cidr nor infra/ec2/terraform.tfvars is set (infra/README.md); the owner runs it by hand and records exit 0 (D39): ${command_line}"
    return
  fi
  # -lockfile=readonly: the backstop reads the committed lock file and never
  # rewrites it (infra/README.md, "Provider bumps and the lock file").
  if ! terraform -chdir="${REPO_ROOT}/infra/ec2" init -input=false -no-color -lockfile=readonly \
      >"${SCRATCH}/terraform-init.log" 2>&1; then
    teardown_fail "terraform -chdir=infra/ec2 init failed; to see why, run it by hand: terraform -chdir=infra/ec2 init -input=false -lockfile=readonly"
    return
  fi
  terraform -chdir="${REPO_ROOT}/infra/ec2" plan -input=false -lock=false -no-color -detailed-exitcode \
    >"${SCRATCH}/terraform-plan.log" 2>&1 || code=$?
  case "$code" in
    0) echo "ok: terraform -chdir=infra/ec2 plan exited 0: infra/ec2 and its state untouched (D39)" ;;
    2) teardown_fail "terraform -chdir=infra/ec2 plan exited 2: changes pending in infra/ec2; read the plan by hand, never in a shared log (it carries the SSH CIDR)" ;;
    *) teardown_fail "terraform -chdir=infra/ec2 plan exited ${code}" ;;
  esac
}

# D39's frozen form, with the hours measured: cluster create to delete,
# rounded up to the tenth.
cost_line() {
  local seconds hours dollars
  [ -n "$T0" ] || return 0
  seconds=$((${DELETED_AT:-$(now)} - T0))
  hours="$(awk -v s="$seconds" 'BEGIN { printf "%.1f", int((s + 359) / 360) / 10 }')"
  dollars="$(awk -v h="$hours" -v r="$HOURLY_ARITHMETIC_USD" 'BEGIN { printf "%.2f", h * r }')"
  echo "cost: ${hours}h wall clock × verified rates, verified next day (D39)"
  echo "note: D39's arithmetic, not a measurement: ≈\$${HOURLY_ARITHMETIC_USD}/h (the \$${EKS_RATE_USD} control plane verified above, plus the t3.medium) × ${hours}h ≈ \$${dollars}; under \$0.50 expected at the 3h bound, \$1 the expected-worst arithmetic, not a mechanism"
}

closing_line() {
  local at
  if [ -n "$T0" ]; then at=$((T0 + 86400)); else at=$(($(now) + 86400)); fi
  echo "next: T+24h: after $(utc_of "$at"), run AWS_PROFILE=${AWS_PROFILE} deploy/eks/demo.sh sweep ${CLUSTER_NAME} and record it, with the billing check, on #92 (D39)"
}

# teardown complete|failed|standalone. complete: the D36-shaped in-cluster
# steps on a healthy demo. failed: teardown first, from the EXIT trap — the
# cluster delete straight away, since nothing in the cluster created anything
# outside it (no LoadBalancer, no volume) and its delete removes it all.
# standalone: the teardown mode, for a run that is gone.
teardown() {
  # Ignore first, then mark: a TERM that lands before this line still finds
  # torn_down at 0, and the EXIT trap runs the whole teardown.
  trap '' INT TERM
  set +e
  trap - ERR
  torn_down=1
  stop_watchdog
  stop_port_forwards
  case "$1" in
    complete)
      echo "ok: teardown starts ($(elapsed)), D36-shaped; INT and TERM are ignored until it ends"
      in_cluster_teardown
      ;;
    failed)
      echo "teardown: first ($(elapsed)): the in-cluster D36 steps are skipped: eksctl delete cluster removes every object in the cluster, and none of them created anything outside it; INT and TERM are ignored until it ends"
      ;;
    standalone)
      echo "teardown: standalone, for ${CLUSTER_NAME}: eksctl delete cluster --wait, then the sweep; INT and TERM are ignored until it ends"
      ;;
  esac
  delete_cluster
  if [ "$1" = standalone ]; then remove_leftover_scratch; else remove_kubeconfig; fi
  sweep "$CLUSTER_NAME"
  terraform_backstop
  cost_line
  closing_line
}

# --- modes ------------------------------------------------------------------------

mode_run() {
  local tip
  [[ "$STABLE_SHA" =~ ^[0-9a-f]{40}$ ]] || die "the stable SHA must be a full 40-character lowercase commit SHA"
  [[ "$CANARY_SHA" =~ ^[0-9a-f]{40}$ ]] || die "the canary SHA must be a full 40-character lowercase commit SHA"
  [ "$CANARY_SHA" != "$STABLE_SHA" ] || die "the canary SHA must differ from the stable SHA"
  require_tools aws kubectl curl jq git tar
  require_profile
  identity
  test_seam_note
  new_cluster_name
  make_scratch
  echo "ok: run: cluster ${CLUSTER_NAME} in ${REGION}; stable ${STABLE_SHA}; canary ${CANARY_SHA}; started $(utc_of "$(now)")"

  install_pinned_tools
  check_kubectl
  verify_eks_version
  verify_eks_rate
  tip="$(git -C "$REPO_ROOT" rev-parse --verify --quiet origin/main 2>/dev/null)" \
    || fail "no local origin/main to check the SHAs against (git fetch origin first). Nothing was created."
  verify_on_main "$STABLE_SHA" "$tip"
  verify_on_main "$CANARY_SHA" "$tip"
  preflight_images
  render_cluster_config

  create_cluster
  assert_node
  assert_server_version
  apply_namespace_and_redis
  install_chart
  assert_operator
  apply_servingdeployment
  serve_stable
  window_valve
  if [ "$valve_used" -eq 0 ]; then
    open_window
    canary_serving
    shadow_as_expected
    close_window
  fi

  teardown complete
  if [ "$teardown_failures" -eq 0 ] && [ "$sweep_failures" -eq 0 ]; then
    if [ "$valve_used" -eq 1 ]; then
      echo "ok: demo complete: the window segment cut by the T+2h valve (D39); teardown, sweep and backstop green"
    else
      echo "ok: demo complete: the full scope; teardown, sweep and backstop green"
    fi
    exit 0
  fi
  echo "fail: the demo ran but its teardown is not green: ${teardown_failures} teardown failure(s), ${sweep_failures} sweep line(s) not absent; re-run: AWS_PROFILE=${AWS_PROFILE} deploy/eks/demo.sh teardown ${CLUSTER_NAME}" >&2
  exit 1
}

mode_sweep() {
  require_cluster_name
  require_tools aws
  require_profile
  identity
  test_seam_note
  sweep "$CLUSTER_NAME"
  echo "next: record this sweep and the billing check — the day's charges for EKS and EC2 in ${REGION}, from the AWS Billing console — on #92 as the T+24h line (D39)"
  [ "$sweep_failures" -eq 0 ] || exit 1
}

mode_teardown() {
  require_cluster_name
  require_tools aws curl jq tar
  require_profile
  identity
  test_seam_note
  make_scratch
  install_pinned_tools
  teardown standalone
  if [ "$teardown_failures" -eq 0 ] && [ "$sweep_failures" -eq 0 ]; then
    echo "ok: teardown complete for ${CLUSTER_NAME}: sweep and backstop green"
    exit 0
  fi
  echo "fail: teardown of ${CLUSTER_NAME} is not green: ${teardown_failures} teardown failure(s), ${sweep_failures} sweep line(s) not absent" >&2
  exit 1
}

MODE="${1:-}"
case "$MODE" in
  run)
    [ "$#" -eq 3 ] || usage
    STABLE_SHA="$2"
    CANARY_SHA="$3"
    mode_run
    ;;
  sweep)
    [ "$#" -eq 2 ] || usage
    CLUSTER_NAME="$2"
    mode_sweep
    ;;
  teardown)
    [ "$#" -eq 2 ] || usage
    CLUSTER_NAME="$2"
    mode_teardown
    ;;
  *)
    usage
    ;;
esac
