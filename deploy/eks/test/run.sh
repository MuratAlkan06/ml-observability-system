#!/usr/bin/env bash
#
# The stub harness for deploy/eks/demo.sh — Phase 3 H2 (docs/PLAN.md D39).
#
#   deploy/eks/test/run.sh          # every scenario
#   deploy/eks/test/run.sh valve    # the scenarios whose name contains "valve"
#
# Runs demo.sh against the fakes in fakes/ — aws, eksctl, kubectl, helm,
# terraform and curl — with no network, no AWS account and no cluster. Before
# anything runs it checks that each of those six names resolves to its fake on
# the test PATH and to nothing real, and refuses otherwise. demo.sh's two test
# seams are set (DEMO_TEST_CLOCK_FILE, DEMO_TEST_TOOLS_DIR), which it records
# on a `note:` line: these transcripts are harness runs, not evidence.
#
# Each scenario's transcript, its fakes' call log and its inventory stay under
# DEMO_TEST_OUT (a fresh temporary directory by default), printed at the end.
# Not CI: deploy/eks/ is in no workflow and no path filter (D39). Needs bash,
# jq, git and this repository's origin/main.
set -euo pipefail
set +x

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${HERE}/../../.." && pwd)"
DEMO="${REPO_ROOT}/deploy/eks/demo.sh"
FAKES="${HERE}/fakes"
FILTER="${1:-}"

# Two real commits on main, the H1 tip and its parent.
STABLE=fb230c9a0d65358886e2f32d4c0cdf88ec845fde
CANARY=0126342129d11d1aa409400ab85e1949db2626c6
NAME=mlobs-demo-0123abcd
CLOCK_BASE=1790000000
FAMILIES=(alpha.eksctl.io/cluster-name eks:cluster-name "kubernetes.io/cluster/${NAME}")

OUT="${DEMO_TEST_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/eks-demo-test.XXXXXX")}"
mkdir -p "$OUT"

die() {
  echo "error: $1" >&2
  exit 1
}

# --- the test PATH, and the guard on it --------------------------------------

command -v jq >/dev/null 2>&1 || die "jq not found on PATH"
REALBIN="${OUT}/realbin"
mkdir -p "$REALBIN"
ln -sf "$(command -v jq)" "${REALBIN}/jq"
TEST_PATH="${FAKES}:${REALBIN}:/usr/bin:/bin"
NO_TF_FAKES="${OUT}/fakes-without-terraform"
mkdir -p "$NO_TF_FAKES"
for tool in aws eksctl kubectl helm curl; do ln -sf "${FAKES}/${tool}" "${NO_TF_FAKES}/${tool}"; done
NO_TF_PATH="${NO_TF_FAKES}:${REALBIN}:/usr/bin:/bin"

for tool in aws eksctl kubectl helm terraform curl; do
  resolved="$(PATH="$TEST_PATH" command -v "$tool" || true)"
  [ "$resolved" = "${FAKES}/${tool}" ] \
    || die "refusing to run: ${tool} resolves to '${resolved}' on the test PATH, not the fake"
done
for tool in aws eksctl kubectl helm terraform; do
  resolved="$(PATH="$NO_TF_PATH" command -v "$tool" || true)"
  case "$tool" in
    terraform) [ -z "$resolved" ] || die "refusing to run: a real terraform at ${resolved} is on the test PATH" ;;
    *) [ "$resolved" = "${NO_TF_FAKES}/${tool}" ] || die "refusing to run: ${tool} resolves to '${resolved}'" ;;
  esac
done
for tool in git sed awk od mktemp tar; do
  PATH="$TEST_PATH" command -v "$tool" >/dev/null 2>&1 || die "${tool} not found under /usr/bin or /bin"
done
for sha in "$STABLE" "$CANARY"; do
  git -C "$REPO_ROOT" merge-base --is-ancestor "$sha" origin/main 2>/dev/null \
    || die "fixture SHA ${sha} is not on this checkout's origin/main (git fetch origin first)"
done

# --- scenarios ------------------------------------------------------------------

passed=0
failed=0
ran=0
current=""
state=""
scenario_ok=1
scenario_env=()
test_path="$TEST_PATH"

base_inventory() {
  cat <<EOF
instance i-0p1host00000001 running project=mlobs Name=mlobs-host
sg sg-0p1host00000001 - project=mlobs Name=mlobs-app
eni eni-0p1host0000001 in-use project=mlobs
vpc vpc-0default000001 available Name=default
role mlobs-deploy - project=mlobs
role mlobs-tf-plan - project=mlobs
role AWSServiceRoleForSupport -
oidc arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com - project=mlobs
stack mlobs-unrelated CREATE_COMPLETE project=mlobs
lb arn:aws:elasticloadbalancing:us-west-2:123456789012:loadbalancer/app/mlobs-p1/0a1b2c3d - project=mlobs
instance i-0otherdemo000001 running eks:cluster-name=mlobs-demo-ffffffff kubernetes.io/cluster/mlobs-demo-ffffffff=owned
stack eksctl-mlobs-demo-ffffffff-cluster CREATE_COMPLETE alpha.eksctl.io/cluster-name=mlobs-demo-ffffffff
loggroup /aws/eks/mlobs-demo-ffffffff/cluster -
EOF
}

# scenario NAME: a fresh state directory, clock and inventory; true if NAME
# passes the filter.
scenario() {
  current="$1"
  case "$current" in *"$FILTER"*) ;; *) return 1 ;; esac
  state="${OUT}/${current}"
  rm -rf "$state"
  mkdir -p "${state}/tmp"
  : >"${state}/calls.log"
  echo "$CLOCK_BASE" >"${state}/clock"
  base_inventory >"${state}/inventory"
  scenario_ok=1
  scenario_env=()
  test_path="$TEST_PATH"
  ran=$((ran + 1))
}

# run_demo ARGS...: demo.sh under the fakes, with a 120 s guard.
run_demo() {
  local status=0 pid guard
  env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u KUBECONFIG \
    -u TF_VAR_ssh_ingress_cidr -u DEMO_TEST_TOOLS_DIR -u AWS_PROFILE \
    PATH="$test_path" TMPDIR="${state}/tmp" \
    DEMO_TEST_CLOCK_FILE="${state}/clock" FAKE_STATE="$state" FAKE_CLOCK_BASE="$CLOCK_BASE" \
    POLL_SECONDS=0 WATCHDOG_POLL_SECONDS=1 \
    ${scenario_env[@]+"${scenario_env[@]}"} \
    "$DEMO" "$@" >"${state}/transcript" 2>&1 &
  pid=$!
  # The guard polls, so it ends within a second of the run it guards.
  (
    for _ in $(seq 1 120); do
      kill -0 "$pid" 2>/dev/null || exit 0
      sleep 1
    done
    kill -TERM "$pid" 2>/dev/null
  ) >/dev/null 2>&1 &
  guard=$!
  wait "$pid" || status=$?
  kill "$guard" 2>/dev/null || true
  wait "$guard" 2>/dev/null || true
  echo "$status" >"${state}/status"
}

# The common environment of a demo run: the profile, the tools seam and a
# terraform variable, each dropped by the scenarios that test its absence.
standard_env() {
  scenario_env=(AWS_PROFILE=mlobs-demo-admin DEMO_TEST_TOOLS_DIR="$FAKES" TF_VAR_ssh_ingress_cidr=203.0.113.7/32)
}

miss() {
  echo "    miss: $1"
  scenario_ok=0
}

expect_status() {
  local got
  got="$(cat "${state}/status")"
  case "$1" in
    nonzero) [ "$got" -ne 0 ] || miss "exit status 0, expected nonzero" ;;
    *) [ "$got" = "$1" ] || miss "exit status ${got}, expected $1" ;;
  esac
}

expect_line() {
  grep -qE -- "$1" "${state}/transcript" || miss "no line matching: $1"
}

expect_no_line() {
  ! grep -qE -- "$1" "${state}/transcript" || miss "a line matching: $1"
}

expect_first_line() {
  head -n 1 "${state}/transcript" | grep -qE -- "$1" || miss "the first line does not match: $1"
}

expect_call() {
  grep -qE -- "$1" "${state}/calls.log" || miss "no call matching: $1"
}

expect_no_call() {
  ! grep -qE -- "$1" "${state}/calls.log" || miss "a call matching: $1"
}

# expect_order ERE...: the lines appear in this order (others may sit between).
expect_order() {
  printf '%s\n' "$@" >"${state}/order"
  awk 'NR == FNR { pattern[++n] = $0; next }
       i < n && $0 ~ pattern[i + 1] { i++ }
       END { if (i < n) { print pattern[i + 1]; exit 1 } }' \
    "${state}/order" "${state}/transcript" >"${state}/order.missing" \
    || miss "out of order or missing, from: $(cat "${state}/order.missing")"
}

finish() {
  # A text tool erroring inside the script is a bug even when the scenario's
  # own lines come out right: the first one this harness found made every
  # sweep line read "none" on macOS.
  if grep -qE '^(grep|awk|sed|jq|tr|sort|cut|od|date|bash|.*demo\.sh: line [0-9]+): ' "${state}/transcript"; then
    miss "a tool error in the transcript: $(grep -m 1 -E '^(grep|awk|sed|jq|tr|sort|cut|od|date|bash|.*demo\.sh: line [0-9]+): ' "${state}/transcript")"
  fi
  if [ "$scenario_ok" -eq 1 ]; then
    echo "ok: ${current}"
    passed=$((passed + 1))
  else
    echo "fail: ${current} (transcript ${state}/transcript)"
    failed=$((failed + 1))
  fi
}

# --- run: the full happy path ---------------------------------------------------

# Regexes: a \$ in single quotes is a literal dollar sign, on purpose.
# shellcheck disable=SC2016
HAPPY_ORDER=(
  '^identity: aws sts get-caller-identity: arn:aws:iam::123456789012:user/mlobs-demo-admin '
  '^ok: identity is not the account root$'
  '^note: test seams set: DEMO_TEST_CLOCK_FILE DEMO_TEST_TOOLS_DIR; this transcript is a harness run, not evidence$'
  "^ok: run: cluster mlobs-demo-[0-9a-f]+ in us-west-2; stable ${STABLE}; canary ${CANARY}; "
  '^ok: helm v3\.22\.0, pinned \(D38\)'
  '^ok: eksctl 0\.230\.0, pinned \(D39\)'
  '^ok: kubectl client v1\.36\.1, within one minor version of 1\.36$'
  '^ok: EKS 1\.36 in us-west-2: STANDARD_SUPPORT until '
  '^ok: EKS control plane, standard support: \$0\.10/h in us-west-2 \(USW2-AmazonEKS-Hours:perCluster at 0\.1000000000 USD/h; source: the AWS Price List, https://pricing'
  "^ok: ${STABLE} is on main"
  "^ok: ${CANARY} is on main"
  "^ok: preflight: ghcr\.io/muratalkan06/mlobs-operator:${STABLE} "
  "^ok: preflight: ghcr\.io/muratalkan06/mlobs-api:${STABLE} "
  "^ok: preflight: ghcr\.io/muratalkan06/mlobs-api:${CANARY} "
  '^ok: deploy/eks/cluster\.yaml rendered for mlobs-demo-[0-9a-f]+$'
  '^ok: teardown-first armed: '
  '^ok: T0 [0-9TZ:-]+: eksctl create cluster starts; the T\+2h window-segment valve at .*, the hard T\+3h bound at '
  '^ok: eksctl create cluster: mlobs-demo-[0-9a-f]+ created \(T\+0h18m\); kubeconfig .*/kubeconfig, a scratch file, never ~/\.kube/config$'
  '^ok: node ip-192-168-31-7\.us-west-2\.compute\.internal Ready: t3\.medium, public IPv4 34\.217\.0\.10 '
  '^ok: kubectl version: server v1\.36\.2-eks-3a1b2c4, Kubernetes 1\.36 '
  '^ok: deploy/k3s/manifests/00-namespace\.yaml and 11-redis\.yaml applied verbatim '
  "^ok: helm install mlobs-operator -n mlobs: deploy/helm/mlobs-operator at image\.prefix ghcr\.io/muratalkan06, image\.tag ${STABLE}; crd/servingdeployments\.serving\.mlobs\.dev Established"
  "^ok: operator Ready: deployment/operator at ghcr\.io/muratalkan06/mlobs-operator:${STABLE}; lease/5bb4b9ad\.mlobs\.dev held by pod/operator-6b8f9c7d5-x2x7q "
  "^ok: deployment/api created and controlled by servingdeployment/api at ghcr\.io/muratalkan06/mlobs-api:${STABLE}; Ready at observedGeneration == generation \(1\)$"
  '^ok: deployment/api /health 200 through a port-forward$'
  '^ok: deployment/api /predict 200 with request_id, label, confidence through a port-forward$'
  '^ok: valve not used: T\+0h18m at the window segment, under T\+2h \(D39\)$'
  "^ok: window open \(T\+0h18m\): CanaryActive True at generation 2; deployment/api-canary at ghcr\.io/muratalkan06/mlobs-api:${CANARY}, controlled by servingdeployment/api$"
  '^ok: deployment/api-canary /predict 200 with request_id, label, confidence through a port-forward$'
  '^ok: canary serving, pod-scoped \(D39\): the canary.s own mlobs_predictions_total 0 -> 1 across the port-forward to deployment/api-canary$'
  '^note: stated, not demonstrated here: the D34 per-connection split and the NodePort are k3s properties'
  '^ok: ShadowPaused False, reason ShadowNotFound: expected, no shadow StatefulSet exists on EKS'
  '^ok: the constant close patch sent: \{"spec":\{"canaryImageTag":null,"canaryReplicas":0\}\} \(apply\.sh.s CLOSE_WINDOW_PATCH, read from it verbatim\)$'
  '^ok: deployment/api /health 200 through a port-forward$'
  '^ok: steady \(T\+0h18m\): CanaryActive False and Ready True at generation 3; deployment/api-canary at 0 with no pods; deployment/api serving$'
  '^ok: teardown starts \(T\+0h18m\), D36-shaped'
  '^ok: teardown: servingdeployment/api deleted$'
  '^ok: teardown: deployment/api and deployment/api-canary garbage-collected through their ownerReferences to servingdeployment/api$'
  '^ok: teardown: helm uninstall mlobs-operator -n mlobs$'
  '^ok: teardown: crd/servingdeployments\.serving\.mlobs\.dev still present after helm uninstall: crds/ is install-only \(D38\)$'
  '^ok: teardown: crd/servingdeployments\.serving\.mlobs\.dev deleted explicitly, last$'
  '^ok: eksctl delete cluster --wait: mlobs-demo-[0-9a-f]+ deleted \(T\+0h28m\)$'
  '^ok: scratch kubeconfig deleted: .*/kubeconfig$'
  '^ok: sweep: mlobs-demo-[0-9a-f]+ in us-west-2, by the three cluster-scoped tag families only: '
  '^ok: sweep: EC2 instances \(any state but terminated\): none$'
  '^ok: sweep: security groups: none$'
  '^ok: sweep: network interfaces \(ENIs, the DependencyViolation class\): none$'
  '^ok: sweep: launch templates: none$'
  '^ok: sweep: CloudFormation stacks \(every status but DELETE_COMPLETE, terminal failures included\): none$'
  '^ok: sweep: VPCs: none$'
  '^ok: sweep: load balancers: none \(none is created; asserted anyway\)$'
  '^ok: sweep: IAM roles: none$'
  '^ok: sweep: CloudWatch log group /aws/eks/mlobs-demo-[0-9a-f]+/cluster: none \(expected-absent by construction: control-plane logging off\)$'
  '^ok: sweep: IAM OIDC providers: none \(expected-absent by construction: withOIDC false\)$'
  '^ok: sweep: all-absent for mlobs-demo-[0-9a-f]+: 10 lines \(D39\)$'
  '^ok: terraform -chdir=infra/ec2 plan exited 0: infra/ec2 and its state untouched \(D39\)$'
  '^cost: 0\.5h wall clock × verified rates, verified next day \(D39\)$'
  "^note: D39's arithmetic, not a measurement: "
  '^next: T\+24h: after [0-9TZ:-]+, run AWS_PROFILE=mlobs-demo-admin deploy/eks/demo.sh sweep mlobs-demo-[0-9a-f]+ and record it, with the billing check, on #92 \(D39\)$'
  '^ok: demo complete: the full scope; teardown, sweep and backstop green$'
)

if scenario run-happy-path; then
  standard_env
  run_demo run "$STABLE" "$CANARY"
  expect_status 0
  expect_first_line '^identity: aws sts get-caller-identity: '
  expect_order "${HAPPY_ORDER[@]}"
  expect_no_line '^(fail|error|valve|skip):'
  # The scratch kubeconfig: what eksctl wrote, what every kubectl and helm
  # call read, and gone at the end, with the whole scratch directory.
  kubeconfig="$(sed -n 's/^ok: scratch kubeconfig deleted: //p' "${state}/transcript")"
  case "$kubeconfig" in "${state}/tmp/mlobs-demo-"*/kubeconfig) ;; *) miss "kubeconfig '${kubeconfig}' is not in the scratch directory" ;; esac
  [ ! -e "$kubeconfig" ] || miss "the scratch kubeconfig still exists"
  [ -z "$(ls -A "${state}/tmp")" ] || miss "the scratch directory was not removed"
  expect_call "^eksctl[[:space:]]create cluster --config-file .*/cluster\.yaml --kubeconfig ${kubeconfig} --timeout 40m$"
  grep '^kubectl[[:space:]]' "${state}/calls.log" | grep -v '[[:space:]]version --client -o json[[:space:]]' | grep -vqF "KUBECONFIG=${kubeconfig}" \
    && miss "a kubectl call ran under another KUBECONFIG"
  expect_call "^helm[[:space:]]install mlobs-operator ${REPO_ROOT}/deploy/helm/mlobs-operator --namespace mlobs --set image\.prefix=ghcr\.io/muratalkan06 --set image\.tag=${STABLE} --wait --timeout [0-9]+s$"
  expect_call "^kubectl[[:space:]]apply -f ${REPO_ROOT}/deploy/k3s/manifests/00-namespace\.yaml[[:space:]]"
  expect_call "^kubectl[[:space:]]apply -f ${REPO_ROOT}/deploy/k3s/manifests/11-redis\.yaml[[:space:]]"
  expect_call "\"canaryImageTag\":\"${CANARY}\",\"canaryReplicas\":1"
  expect_call '^eksctl[[:space:]]delete cluster --name mlobs-demo-[0-9a-f]+ --region us-west-2 --wait --timeout 40m$'
  expect_call '^terraform[[:space:]]-chdir=.*/infra/ec2 init -input=false -no-color -lockfile=readonly$'
  expect_call '^terraform[[:space:]]-chdir=.*/infra/ec2 plan -input=false -lock=false -no-color -detailed-exitcode$'
  # Teardown order (D36): the CR, the release, the CRD, then the cluster.
  awk -F'\t' '
    $2 ~ /^--namespace mlobs delete servingdeployment\/api/ { cr = NR }
    $1 == "helm" && $2 ~ /^uninstall/ { helm = NR }
    $2 ~ /^delete crd / { crd = NR }
    $1 == "eksctl" && $2 ~ /^delete cluster/ { cluster = NR }
    END { exit !(cr && cr < helm && helm < crd && crd < cluster) }' "${state}/calls.log" \
    || miss "the teardown calls are not in D36 order: CR, helm uninstall, CRD, cluster"
  # The sweep swept what the run created: its terminated node is listed in
  # the inventory and not reported.
  grep -qE '^instance i-0[0-9a-f]+0node terminated ' "${state}/inventory" || miss "no terminated node left listed in the inventory"
  expect_no_line 'i-0[0-9a-f]+0node'
  finish
fi

# --- run: refusals before anything exists ---------------------------------------

if scenario run-refuses-root-identity; then
  standard_env
  scenario_env+=(FAKE_IDENTITY_ARN=arn:aws:iam::123456789012:root)
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_first_line '^identity: aws sts get-caller-identity: arn:aws:iam::123456789012:root '
  expect_line '^error: the caller is the account root \(arn:aws:iam::123456789012:root\); D39 rules root out'
  expect_no_line '^ok: identity is not the account root'
  expect_no_call '^(eksctl|helm|kubectl|curl)[[:space:]]'
  expect_no_call '^aws[[:space:]](eks|ec2|cloudformation|iam)'
  finish
fi

if scenario run-refuses-missing-aws-profile; then
  scenario_env=(DEMO_TEST_TOOLS_DIR="$FAKES")
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_first_line '^error: AWS_PROFILE is not set: the demo runs only as the designated non-root admin principal, named by its profile \(D39\)\. Nothing was called\.$'
  [ ! -s "${state}/calls.log" ] || miss "a tool was called without AWS_PROFILE"
  finish
fi

if scenario run-ghcr-404-aborts-before-create; then
  standard_env
  scenario_env+=(FAKE_GHCR_404="muratalkan06/mlobs-api:${CANARY}")
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_line "^ok: preflight: ghcr\.io/muratalkan06/mlobs-operator:${STABLE} "
  expect_line "^ok: preflight: ghcr\.io/muratalkan06/mlobs-api:${STABLE} "
  expect_line "^fail: preflight: ghcr\.io/muratalkan06/mlobs-api:${CANARY} not found"
  expect_line '^fail: preflight: an image the run pulls is missing from GHCR\. Nothing was created\.$'
  expect_line '^note: nothing was created: the run stopped before eksctl create cluster'
  expect_no_line '^ok: T0 '
  expect_no_call '^eksctl[[:space:]]create'
  expect_no_call '^(kubectl[[:space:]][^v]|helm[[:space:]]install)'
  finish
fi

if scenario run-pinned-download-checksum-mismatch; then
  standard_env
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_line '^fail: pinned download: helm-v3\.22\.0-(darwin|linux)-(amd64|arm64)\.tar\.gz has sha256 [0-9a-f]+, not the pinned [0-9a-f]+\. Nothing was created\.$'
  expect_no_call '^(eksctl|helm)[[:space:]]'
  expect_no_line '^ok: T0 '
  finish
fi

if scenario run-eks-not-in-standard-support-aborts; then
  standard_env
  scenario_env+=(FAKE_EKS_STATUS=EXTENDED_SUPPORT)
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_line '^fail: EKS 1\.36 in us-west-2 is EXTENDED_SUPPORT today, not STANDARD_SUPPORT'
  expect_no_call '^eksctl[[:space:]]create'
  finish
fi

if scenario run-eks-rate-drift-aborts; then
  standard_env
  scenario_env+=(FAKE_EKS_RATE=0.1200000000)
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  # shellcheck disable=SC2016
  expect_line '^fail: the EKS standard-support control plane is \$0\.1200000000/h in us-west-2 today, not the \$0\.10/h of D39.s cost arithmetic'
  expect_no_call '^eksctl[[:space:]]create'
  finish
fi

if scenario run-sha-not-on-main-aborts; then
  standard_env
  run_demo run "$STABLE" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  expect_status nonzero
  expect_line '^fail: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa is not a commit in this repository'
  expect_no_call '^eksctl[[:space:]]create'
  finish
fi

# --- run: the bound -------------------------------------------------------------

if scenario run-valve-at-t-plus-2h; then
  standard_env
  # The clock passes T+2h during stable serving; and no terraform here, so
  # the backstop's skip line is exercised too.
  scenario_env+=(FAKE_CLOCK_JUMP_ON="port-forward deployment/api " FAKE_CLOCK_JUMP_TO=7300)
  test_path="$NO_TF_PATH"
  run_demo run "$STABLE" "$CANARY"
  expect_status 0
  expect_order \
    '^ok: deployment/api /predict 200 ' \
    '^valve: T\+2h window-segment cut at T\+2h01m \(D39\): the window segment is skipped; the demo stands at chart install, CR Ready at generation and stable serving, and goes to teardown and sweep$' \
    '^ok: teardown starts \(T\+2h01m\), D36-shaped' \
    '^ok: teardown: servingdeployment/api deleted$' \
    '^ok: teardown: crd/servingdeployments\.serving\.mlobs\.dev deleted explicitly, last$' \
    '^ok: eksctl delete cluster --wait: ' \
    '^ok: sweep: all-absent for ' \
    '^skip: terraform -chdir=infra/ec2 plan: terraform is not on PATH; the owner runs it by hand and records exit 0 \(D39\): ' \
    '^cost: 2\.2h wall clock × verified rates, verified next day \(D39\)$' \
    '^ok: demo complete: the window segment cut by the T\+2h valve \(D39\); teardown, sweep and backstop green$'
  expect_no_line '^ok: window open'
  expect_no_line '^ok: valve not used'
  expect_no_call 'canaryImageTag'
  expect_no_call '^terraform[[:space:]]'
  finish
fi

if scenario run-t-plus-3h-watchdog-mid-demo; then
  standard_env
  # The bound passes inside a call that is still running (the window-open
  # patch, hanging 4 s); the watchdog's TERM stops the demo when it returns.
  scenario_env+=(FAKE_CLOCK_JUMP_ON=canaryImageTag FAKE_CLOCK_JUMP_TO=10900 FAKE_HANG_SECONDS=4)
  run_demo run "$STABLE" "$CANARY"
  expect_status 143
  expect_order \
    '^ok: valve not used: ' \
    '^fail: T\+3h hard bound reached \(T\+3h01m\): the watchdog stopped the demo; teardown first \(D39\)$' \
    '^teardown: first \(T\+3h01m\): the in-cluster D36 steps are skipped' \
    '^ok: eksctl delete cluster --wait: mlobs-demo-[0-9a-f]+ deleted \(T\+3h11m\)$' \
    '^ok: scratch kubeconfig deleted: ' \
    '^ok: sweep: all-absent for ' \
    '^ok: terraform -chdir=infra/ec2 plan exited 0' \
    '^cost: 3\.2h wall clock × verified rates, verified next day \(D39\)$' \
    '^next: T\+24h: ' \
    '^fail: the run did not complete; teardown-first ran: 0 teardown failure\(s\), 0 sweep line\(s\) not absent$'
  expect_no_line '^ok: window open'
  expect_no_line '^ok: teardown starts'
  expect_no_call 'delete servingdeployment/api'
  expect_no_call '^helm[[:space:]]uninstall'
  expect_call '^eksctl[[:space:]]delete cluster '
  finish
fi

if scenario run-t-plus-3h-checkpoint-mid-demo; then
  standard_env
  # The same bound with the watchdog asleep: the next step's checkpoint stops
  # the demo instead.
  scenario_env+=(FAKE_CLOCK_JUMP_ON=canaryImageTag FAKE_CLOCK_JUMP_TO=10900 WATCHDOG_POLL_SECONDS=60)
  run_demo run "$STABLE" "$CANARY"
  expect_status 1
  expect_order \
    '^fail: T\+3h hard bound reached before: CanaryActive True at the current generation \(T\+3h01m\); teardown first \(D39\)$' \
    '^teardown: first \(T\+3h01m\)' \
    '^ok: eksctl delete cluster --wait: ' \
    '^ok: sweep: all-absent for '
  expect_no_call 'delete servingdeployment/api'
  finish
fi

if scenario run-create-failure-tears-down-first; then
  standard_env
  scenario_env+=(FAKE_EKSCTL_CREATE_FAIL=1)
  run_demo run "$STABLE" "$CANARY"
  expect_status nonzero
  expect_order \
    '^ok: T0 ' \
    '^fail: eksctl create cluster mlobs-demo-[0-9a-f]+ failed \(T\+0h05m\)$' \
    '^teardown: first \(T\+0h05m\)' \
    '^ok: eksctl delete cluster --wait: ' \
    '^ok: sweep: CloudFormation stacks .*: none$' \
    '^ok: sweep: all-absent for '
  expect_no_call '^kubectl[[:space:]][^v]'
  finish
fi

if scenario run-sweep-failure-fails-the-run; then
  standard_env
  scenario_env+=(FAKE_EKSCTL_DELETE_LEAVES_ENI=1)
  run_demo run "$STABLE" "$CANARY"
  expect_status 1
  expect_line '^fail: sweep: network interfaces \(ENIs, the DependencyViolation class\): PRESENT: eni-0stranded0000001$'
  expect_line '^fail: the demo ran but its teardown is not green: 0 teardown failure\(s\), 1 sweep line\(s\) not absent'
  expect_no_line '^ok: demo complete'
  finish
fi

# --- sweep ----------------------------------------------------------------------

# A clean account for NAME: its node's instance terminated but still listed,
# another demo cluster's resources live, and the P1 host's project-tagged ones.
clean_inventory() {
  base_inventory
  echo "instance i-0terminated00001 terminated eks:cluster-name=${NAME} kubernetes.io/cluster/${NAME}=owned"
}

if scenario sweep-clean-account; then
  clean_inventory >"${state}/inventory"
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo sweep "$NAME"
  expect_status 0
  expect_first_line '^identity: aws sts get-caller-identity: '
  expect_line "^ok: sweep: all-absent for ${NAME}: 10 lines \(D39\)$"
  expect_line '^next: record this sweep and the billing check'
  [ "$(grep -c '^ok: sweep: .*: none' "${state}/transcript")" -eq 10 ] || miss "not 10 all-absent lines"
  expect_no_line 'i-0terminated00001|i-0otherdemo000001|mlobs-demo-ffffffff|p1host|mlobs-deploy|mlobs-tf-plan|token\.actions|mlobs-unrelated|mlobs-p1'
  finish
fi

# The load-bearing negative: one ENI the delete left behind must fail the sweep.
if scenario sweep-stranded-eni-fails; then
  clean_inventory >"${state}/inventory"
  echo "eni eni-0stranded0000001 available kubernetes.io/cluster/${NAME}=owned" >>"${state}/inventory"
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo sweep "$NAME"
  expect_status 1
  expect_line '^fail: sweep: network interfaces \(ENIs, the DependencyViolation class\): PRESENT: eni-0stranded0000001$'
  expect_line '^ok: sweep: VPCs: none$'
  expect_line '^ok: sweep: IAM OIDC providers: none '
  expect_line "^fail: sweep: 1 of 10 lines not absent for ${NAME}; "
  expect_no_line '^ok: sweep: all-absent'
  finish
fi

if scenario sweep-delete-failed-stack-fails; then
  clean_inventory >"${state}/inventory"
  echo "stack eksctl-${NAME}-cluster DELETE_FAILED alpha.eksctl.io/cluster-name=${NAME}" >>"${state}/inventory"
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo sweep "$NAME"
  expect_status 1
  expect_line "^fail: sweep: CloudFormation stacks \(every status but DELETE_COMPLETE, terminal failures included\): PRESENT: eksctl-${NAME}-cluster:DELETE_FAILED$"
  finish
fi

if scenario sweep-failed-query-is-not-absence; then
  clean_inventory >"${state}/inventory"
  scenario_env=(AWS_PROFILE=mlobs-demo-admin FAKE_AWS_FAIL_ON=describe-vpcs)
  run_demo sweep "$NAME"
  expect_status 1
  expect_line '^fail: sweep: VPCs: a query failed, so absence is unproven$'
  finish
fi

# The tag filter: every tag key any sweep query sent is one of the three
# cluster-scoped families, and no query names a project tag — while the fake,
# asked for project=mlobs directly, would have returned the P1 host.
if scenario sweep-filters-only-cluster-tag-families; then
  clean_inventory >"${state}/inventory"
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo sweep "$NAME"
  expect_status 0
  grep '^aws[[:space:]]' "${state}/calls.log" >"${state}/aws-calls"
  {
    grep -oE 'Name=tag:[^,]+' "${state}/aws-calls" | sed 's/^Name=tag://'
    grep -oE 'Name=tag-key,Values=[^ ]+' "${state}/aws-calls" | sed 's/^Name=tag-key,Values=//'
    grep -oE "Key=[^=,' ]+" "${state}/aws-calls" | sed 's/^Key=//'
    grep -oE "Key=='[^']+'" "${state}/aws-calls" | sed "s/^Key=='//; s/'\$//"
  } | sort -u >"${state}/tag-keys"
  [ "$(tr '\n' ' ' <"${state}/tag-keys")" = "$(printf '%s\n' "${FAMILIES[@]}" | sort -u | tr '\n' ' ')" ] \
    || miss "the sweep's tag keys are not exactly the three families: $(tr '\n' ' ' <"${state}/tag-keys")"
  ! grep -qi 'project' "${state}/aws-calls" || miss "a sweep query names a project tag"
  # Every EC2 and tagging-API query carried a family filter.
  grep -E '[[:space:]](ec2 describe-|resourcegroupstaggingapi )' "${state}/aws-calls" \
    | grep -vqE "Name=tag:alpha\.eksctl\.io/cluster-name,Values=${NAME}|Name=tag:eks:cluster-name,Values=${NAME}|Name=tag-key,Values=kubernetes\.io/cluster/${NAME}|Key=alpha\.eksctl\.io/cluster-name,Values=${NAME}|Key=eks:cluster-name,Values=${NAME}|Key=kubernetes\.io/cluster/${NAME}" \
    && miss "an EC2 or tagging-API sweep query carried no cluster-family filter"
  control="$(PATH="$TEST_PATH" FAKE_STATE="$state" aws ec2 describe-instances --region us-west-2 \
    --filters Name=tag:project,Values=mlobs --query 'Reservations[].Instances[].InstanceId' --output text)"
  [ "$control" = i-0p1host00000001 ] || miss "negative control: the fake did not return the P1 host for project=mlobs (got '${control}')"
  expect_no_line 'i-0p1host00000001'
  finish
fi

if scenario sweep-refuses-a-foreign-name; then
  scenario_env=(AWS_PROFILE=mlobs-demo-admin)
  run_demo sweep mlobs-host
  expect_status nonzero
  expect_line "^error: the cluster name must be mlobs-demo-<8 hex>, the name a run printed; got 'mlobs-host'$"
  [ ! -s "${state}/calls.log" ] || miss "a tool was called for a foreign name"
  finish
fi

# --- teardown -------------------------------------------------------------------

if scenario teardown-standalone-dead-run; then
  # A dead run: its resources in the account, its scratch kubeconfig on disk.
  printf 'metadata:\n  name: %s\n' "$NAME" >"${state}/cluster.yaml"
  PATH="$TEST_PATH" FAKE_STATE="$state" DEMO_TEST_CLOCK_FILE="${state}/clock" \
    eksctl create cluster --config-file "${state}/cluster.yaml" --kubeconfig "${state}/seed-kubeconfig" >/dev/null
  : >"${state}/calls.log"
  mkdir -p "${state}/tmp/${NAME}.dead01"
  : >"${state}/tmp/${NAME}.dead01/kubeconfig"
  standard_env
  run_demo teardown "$NAME"
  expect_status 0
  expect_order \
    '^identity: aws sts get-caller-identity: ' \
    '^ok: helm v3\.22\.0' \
    '^ok: eksctl 0\.230\.0' \
    "^teardown: standalone, for ${NAME}: eksctl delete cluster --wait, then the sweep" \
    "^ok: eksctl delete cluster --wait: ${NAME} deleted" \
    "^ok: a leftover scratch directory of ${NAME} removed, its kubeconfig with it: ${state}/tmp/${NAME}\.dead01$" \
    "^ok: sweep: all-absent for ${NAME}: 10 lines" \
    '^ok: terraform -chdir=infra/ec2 plan exited 0' \
    "^ok: teardown complete for ${NAME}: sweep and backstop green$"
  [ ! -e "${state}/tmp/${NAME}.dead01" ] || miss "the dead run's scratch directory survived"
  expect_no_call '^kubectl[[:space:]]'
  finish
fi

if scenario teardown-standalone-nothing-there; then
  standard_env
  run_demo teardown "$NAME"
  expect_status 0
  expect_line "^ok: eksctl delete cluster: neither a cluster nor a cluster stack named ${NAME} exists"
  expect_line "^ok: no leftover scratch kubeconfig of ${NAME} under "
  expect_line "^ok: sweep: all-absent for ${NAME}: 10 lines"
  finish
fi

# --- summary ----------------------------------------------------------------------

echo "transcripts: ${OUT}"
[ "$ran" -gt 0 ] || die "no scenario matches '${FILTER}'"
if [ "$failed" -eq 0 ]; then
  echo "ok: ${passed} of ${ran} scenarios passed"
else
  echo "fail: ${failed} of ${ran} scenarios failed"
  exit 1
fi
