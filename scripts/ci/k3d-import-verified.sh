#!/usr/bin/env bash
#
# `k3d image import`, checked on the node and retried — #141 (refs #60).
#
#   [RETRY_SLEEP_SECONDS=5] scripts/ci/k3d-import-verified.sh CLUSTER IMAGE [IMAGE...]
#
# k3d v5.9.0 can drop an image on import and still exit 0: the node logs
# "ctr: open /k3d/images/<name>.tar: no such file or directory", the import
# step goes green, and the job fails minutes later on a workload whose image
# was never there (upstream k3d-io/k3d#1484). A retry keyed on the exit status
# would never fire, so this one is keyed on what landed. Each attempt imports
# what is still missing, then reads the node's containerd with
# `k3s crictl images` and looks for every IMAGE there, not only the ones that
# attempt imported. Three attempts at most, RETRY_SLEEP_SECONDS apart. If an
# image is still missing after the third, the script prints the full listing
# and exits 1; on success it prints the listing's rows for IMAGE, the
# diagnostic K3sSmoke's import step printed before this script existed.
#
# containerd names an imported image by its normalised reference: a bare
# `name:tag` becomes `docker.io/library/name:tag`, which is why the jobs
# deploy with IMAGE_PREFIX=docker.io/library, and crictl prints that name.
# Each IMAGE is normalised the same way before it is looked for.
#
# Only server-0 is read. Every cluster ci.yml creates is a single server node
# with no agents; a cluster with more nodes would need each one checked.
#
# Every k3d import in ci.yml goes through here — K3sSmoke's two, OperatorE2E's
# and RollbackRehearsal's — so the K3sPaths filter lists scripts/ci/ for all
# three jobs.
set -euo pipefail

RETRY_SLEEP_SECONDS="${RETRY_SLEEP_SECONDS:-5}"
ATTEMPTS=3

die() {
  echo "error: $1" >&2
  exit 2
}

# Docker's reference normalisation, as much of it as a name:tag needs: no tag
# means :latest; a first path component that has no dot or colon and is not
# localhost names no registry, so docker.io is prefixed; a one-component name
# lives under library/.
normalise() {
  local ref="$1"
  case "${ref##*/}" in
    *:*) ;;
    *) ref="${ref}:latest" ;;
  esac
  case "$ref" in
    */*)
      case "${ref%%/*}" in
        *.* | *:* | localhost) ;;
        *) ref="docker.io/${ref}" ;;
      esac
      ;;
    *) ref="docker.io/library/${ref}" ;;
  esac
  printf '%s\n' "$ref"
}

[ "$#" -ge 2 ] || die "usage: $0 CLUSTER IMAGE [IMAGE...]"
cluster="$1"
shift
node="k3d-${cluster}-server-0"

images=("$@")
expected=()
for image in "${images[@]}"; do
  case "$image" in
    *@*) die "digest references are not supported: ${image}" ;;
  esac
  expected+=("$(normalise "$image")")
done

pending=("${images[@]}")
attempt=1
while :; do
  echo "attempt ${attempt}/${ATTEMPTS}: k3d image import -c ${cluster} ${pending[*]}"
  status=0
  k3d image import -c "$cluster" "${pending[@]}" || status=$?
  if [ "$status" -ne 0 ]; then
    echo "::warning::k3d image import exited ${status} on attempt ${attempt}; checking ${node} for what landed"
  fi

  status=0
  listing="$(docker exec "$node" k3s crictl images)" || status=$?
  if [ "$status" -ne 0 ]; then
    echo "::warning::crictl images on ${node} exited ${status} on attempt ${attempt}; counting every image as missing"
    listing=""
  fi
  # IMAGE and TAG are the listing's first two columns, below one header row.
  present="$(printf '%s\n' "$listing" | awk 'NR > 1 { print $1 ":" $2 }')"

  missing=()
  missing_refs=()
  for i in "${!images[@]}"; do
    case $'\n'"${present}"$'\n' in
      *$'\n'"${expected[$i]}"$'\n'*) ;;
      *)
        missing+=("${images[$i]}")
        missing_refs+=("${expected[$i]}")
        ;;
    esac
  done

  if [ "${#missing[@]}" -eq 0 ]; then
    echo "ok: all ${#images[@]} images present on ${node} after attempt ${attempt}/${ATTEMPTS}"
    printf '%s\n' "$listing" | awk -v want="${expected[*]}" '
      BEGIN { n = split(want, w, " "); for (i = 1; i <= n; i++) keep[w[i]] = 1 }
      NR == 1 || (($1 ":" $2) in keep)'
    exit 0
  fi

  if [ "$attempt" -ge "$ATTEMPTS" ]; then
    break
  fi
  echo "::warning::attempt ${attempt}/${ATTEMPTS}: ${#missing[@]} of ${#images[@]} images missing on ${node} (k3d-io/k3d#1484): ${missing_refs[*]}; retrying in ${RETRY_SLEEP_SECONDS}s"
  pending=("${missing[@]}")
  sleep "$RETRY_SLEEP_SECONDS"
  attempt=$((attempt + 1))
done

echo "::error::${#missing[@]} of ${#images[@]} images still missing on ${node} after ${ATTEMPTS} attempts of k3d image import (k3d-io/k3d#1484, #141)"
for ref in "${missing_refs[@]}"; do
  echo "missing: ${ref}"
done
echo "crictl images on ${node}:"
printf '%s\n' "$listing"
exit 1
