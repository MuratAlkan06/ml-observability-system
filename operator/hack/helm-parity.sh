#!/usr/bin/env bash
#
# The operator chart against the flattened manifests it packages, and the
# chart's two guards — Phase 3 H1 (docs/PLAN.md D38).
#
#   [HELM=helm] operator/hack/helm-parity.sh
#
# CI's HelmParity job runs exactly this, on the pinned Helm. What it asserts:
#   1. no ServingDeployment anywhere under deploy/helm/: D32's writer set is
#      three, and a chart-rendered CR would be a fourth
#   2. parity: deploy/helm/mlobs-operator rendered by `helm template -n mlobs`
#      at a sentinel prefix and a 40-hex sentinel tag, without --include-crds,
#      is byte-identical to 02-operator-rbac.yaml and 03-operator.yaml with
#      their IMAGE_PREFIX and IMAGE_TAG placeholders sed-rendered to the same
#      sentinels, as apply.sh renders them
#   3. each guard refuses its negative case on its fixed line: a non-40-hex
#      image.tag; a release namespace other than mlobs
#
# The CRD is not rendered here: its parity is the sync check's job, which
# holds the chart's crds/ copy byte-exact against controller-gen
# (hack/manifests.sh).
#
# How the parity compares. The one thing removed is the `# Source:` line Helm
# writes above each document it emits, matched in Helm's own form; the 03
# header has a comment line of its own that begins `# Source:` and is kept.
# Helm also re-orders what it emits, by kind into its install order (so the
# Role ahead of the RoleBinding the flattened file puts first) with
# comment-only documents last, and no template can change that. Both sides
# are therefore cut at their `---` lines and compared as documents, each one
# byte-exact, in one canonical order. Nothing else is normalised: no
# whitespace, no comments, no labels.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
MANIFESTS="${REPO_ROOT}/deploy/k3s/manifests"
HELM_DIR="${REPO_ROOT}/deploy/helm"
CHART="${HELM_DIR}/mlobs-operator"
HELM="${HELM:-helm}"

RELEASE=mlobs-operator
NAMESPACE=mlobs
SENTINEL_PREFIX=sentinel.invalid/helm-parity
SENTINEL_TAG=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef
# The guards' fixed lines, as templates/guards.yaml fails with them.
TAG_REFUSAL='image.tag must be a 40-character lowercase hex commit SHA'
NAMESPACE_REFUSAL='the release namespace must be mlobs (install with -n mlobs)'

die() {
  echo "error: $1" >&2
  exit 1
}

command -v "$HELM" >/dev/null 2>&1 || die "helm not found (HELM=${HELM})"
[ -d "$CHART" ] || die "missing deploy/helm/mlobs-operator"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# --- 1. no ServingDeployment under deploy/helm/ ------------------------------

# A CR is a document whose top-level kind is ServingDeployment; the CRD's own
# `kind: ServingDeployment` under spec.names is indented and does not match.
# Explicit status: a bare `! grep` reads a grep error as "no match".
status=0
grep -rnE '^kind:[[:space:]]*"?ServingDeployment"?[[:space:]]*$' "$HELM_DIR" >&2 || status=$?
case "$status" in
  0) die "a ServingDeployment under deploy/helm/; the chart ships the operator only (D32, D38)" ;;
  1) echo "ok: no ServingDeployment under deploy/helm/" ;;
  *) die "grep exited with status ${status}" ;;
esac

# --- 2. parity ---------------------------------------------------------------

# canonical FILE...: the documents of FILE..., cut at their `---` lines (and
# at each file's end), each document byte-exact, sorted, and written back
# out one per `---`. A document travels through sort as a single line, its
# newlines swapped for \001, which the inputs are checked not to contain.
canonical() {
  if LC_ALL=C grep -q $'\001' "$@"; then
    die "an input holds a \\001 byte, which canonical() uses as its line marker"
  fi
  LC_ALL=C awk '
    FNR == 1 && doc != "" { print doc; doc = "" }
    /^---$/ { if (doc != "") print doc; doc = ""; next }
    { doc = doc $0 "\001" }
    END { if (doc != "") print doc }
  ' "$@" | LC_ALL=C sort | LC_ALL=C awk '{ print "---"; gsub(/\001/, "\n"); printf "%s", $0 }'
}

"$HELM" template "$RELEASE" "$CHART" --namespace "$NAMESPACE" \
  --set image.prefix="$SENTINEL_PREFIX" --set image.tag="$SENTINEL_TAG" \
  >"${work_dir}/chart.raw.yaml"
sed "/^# Source: ${RELEASE}\/templates\/[^ ]*\$/d" "${work_dir}/chart.raw.yaml" >"${work_dir}/chart.yaml"

for name in 02-operator-rbac.yaml 03-operator.yaml; do
  sed -e "s|IMAGE_PREFIX|${SENTINEL_PREFIX}|g" -e "s|IMAGE_TAG|${SENTINEL_TAG}|g" \
    "${MANIFESTS}/${name}" >"${work_dir}/${name}"
done

canonical "${work_dir}/chart.yaml" >"${work_dir}/chart.canonical.yaml"
canonical "${work_dir}/02-operator-rbac.yaml" "${work_dir}/03-operator.yaml" \
  >"${work_dir}/flattened.canonical.yaml"

documents="$(grep -cxF -- '---' "${work_dir}/flattened.canonical.yaml" || true)"
[ "${documents:-0}" -gt 0 ] || die "no documents in the rendered flattened manifests"
if diff -u "${work_dir}/flattened.canonical.yaml" "${work_dir}/chart.canonical.yaml"; then
  echo "ok: helm template -n ${NAMESPACE} matches 02-operator-rbac.yaml and 03-operator.yaml, ${documents} documents byte-exact"
else
  die "the chart's render differs from the flattened manifests (- flattened, + chart); the flattened files are the authority (D38)"
fi

# --- 3. the guards refuse their negative cases -------------------------------

# refuses WHAT LINE HELM_ARGS...: helm template with HELM_ARGS must fail, and
# on LINE.
refuses() {
  local what="$1" line="$2" status=0
  shift 2
  "$HELM" template "$RELEASE" "$CHART" "$@" >/dev/null 2>"${work_dir}/refusal.err" || status=$?
  [ "$status" -ne 0 ] || die "the chart rendered ${what}; its guard should have refused it"
  if ! grep -qF -- "$line" "${work_dir}/refusal.err"; then
    cat "${work_dir}/refusal.err" >&2
    die "the chart refused ${what}, but not on its fixed line: ${line}"
  fi
  echo "ok: the chart refused ${what} on its fixed line: ${line}"
}
refuses "a non-40-hex image.tag" "$TAG_REFUSAL" \
  --namespace "$NAMESPACE" --set image.prefix="$SENTINEL_PREFIX" --set image.tag=deadbee
refuses "a release namespace other than ${NAMESPACE}" "$NAMESPACE_REFUSAL" \
  --namespace other --set image.prefix="$SENTINEL_PREFIX" --set image.tag="$SENTINEL_TAG"

echo "ok: helm parity passed"
