# mlobs-operator

A Helm chart for the `ServingDeployment` operator, version 0.1.0 (Phase 3 H1;
[`docs/PLAN.md`](../../../docs/PLAN.md) D38). It packages exactly what three
flattened manifests in [`deploy/k3s/manifests/`](../../k3s/manifests/)
already describe, and nothing else:

| Chart file | Flattened source | How |
|---|---|---|
| `crds/01-servingdeployment-crd.yaml` | `01-servingdeployment-crd.yaml` | a byte-exact copy of the whole file |
| `templates/02-operator-rbac.yaml` | `02-operator-rbac.yaml` | the file verbatim; no value in it |
| `templates/03-operator.yaml` | `03-operator.yaml` | the file verbatim but for the two seams below |

`templates/guards.yaml` renders nothing; it holds the two guards.

The flattened manifests stay the authority. The k3s path, on the host and in
the pipeline, runs no Helm at all: `deploy/k3s/apply.sh` applies the flattened
files. The chart exists for the ephemeral EKS demonstration (D39) and as the
packaging evidence D17 deferred to this phase. A change to the operator's
manifests is made in the flattened file first (or in the Go markers, then
`make -C operator manifests`) and mirrored here; CI fails until the two agree.

## Install

The namespace comes first, from the flattened manifests, then the chart, into
`mlobs` and nowhere else. The tag is the 40-hex commit SHA of a `main` commit
whose `mlobs-operator` image `GhcrPublish` has pushed.

```bash
kubectl apply -f deploy/k3s/manifests/00-namespace.yaml
helm install mlobs-operator deploy/helm/mlobs-operator -n mlobs \
  --set image.tag=<40-hex sha>
```

The chart ships no `ServingDeployment`. D32's set of spec writers is frozen
at three, and a chart-rendered CR would be a fourth: the CR is
`40-servingdeployment.yaml`, rendered as `apply.sh` renders it.

## Values: two seams, and no third

| Value | Default | Fills |
|---|---|---|
| `image.prefix` | `ghcr.io/muratalkan06` | `IMAGE_PREFIX` in `03-operator.yaml`: the operator's image, and its `--image-prefix` for the api images it writes |
| `image.tag` | none: required | `IMAGE_TAG` in `03-operator.yaml`: the operator image's per-SHA tag |

The namespace is not a value.

## Guards

Each guard fails the render, on every path (`helm template`, `helm lint`,
`helm install`), with a fixed line:

| Refused | Fixed line |
|---|---|
| an `image.tag` that is not 40-character lowercase hex (`^[0-9a-f]{40}$`, the CRD's and the SSM document's pattern, D29), or none | `image.tag must be a 40-character lowercase hex commit SHA` |
| a release namespace other than `mlobs` | `the release namespace must be mlobs (install with -n mlobs)` |

CI's `HelmParity` job renders one negative case per guard and asserts its
line.

## The CRD: `crds/` is install-only

Helm's `crds/` contract, stated plainly:

- **Install only.** `helm install` creates the CRD when it is absent. When it
  is present, whatever its version, Helm skips it.
- **Never upgraded.** `helm upgrade` and `helm rollback` never touch it.
- **Never deleted.** `helm uninstall` leaves the CRD, and every
  `ServingDeployment` in the cluster, in place. Removing it is an explicit
  `kubectl delete crd servingdeployments.serving.mlobs.dev`, last, after the
  `ServingDeployment` is gone: deleting a CRD deletes all of its objects.

This chart has **no CRD upgrade path at 0.1.0**. CRD changes ship through the
flattened manifests: `01-servingdeployment-crd.yaml`, applied by `apply.sh`,
or with `kubectl apply -f` on a cluster the chart was installed into.

## One namespace: a non-goal

The chart installs into `mlobs` only, and that is recorded as its non-goal,
not left as a missing value. The pin is load-bearing twice: the operator
binary's leader-election namespace is a literal, and its Role is shaped for
`mlobs` (D33). The templates also name `metadata.namespace: mlobs` literally,
for byte-parity with the flattened manifests, so a release in another
namespace would put the objects in one namespace and Helm's release records
in another; the guard refuses it instead. Serving more than one namespace
would need the namespace-wide read grant the D33 addendum keeps closed.

## How the chart is held to the flattened manifests

- **`OperatorManifestSync`** (`make -C operator verify-manifests`) checks three
  projections of one Go source, each whole-file and byte-exact: the flattened
  CRD, the flattened Role, and this chart's `crds/` copy. `make -C operator
  manifests` rewrites all three. The check also fails if `crds/` holds any
  other file.
- **`HelmParity`** (`operator/hack/helm-parity.sh`) renders the chart with
  `helm template -n mlobs` at a sentinel prefix and a 40-hex sentinel tag,
  without `--include-crds`, and compares it with `02-operator-rbac.yaml` and
  `03-operator.yaml`, their placeholders sed-rendered to the same sentinels.
  The only thing removed is the `# Source:` line Helm writes above each
  document. Helm emits documents in its own install order, by kind (the Role
  ahead of the RoleBinding that the flattened file puts first) and with
  comment-only documents last, and no template can change that; so the two
  sides are compared document by document, each one byte-exact, in one
  canonical order. Nothing else is normalised. The flattened files' header
  comments ride along verbatim, which Helm emits as comment-only documents
  that Kubernetes never receives. The job also fails on any
  `ServingDeployment` under `deploy/helm/`.
- **No Helm labels.** The templates carry no `app.kubernetes.io/*` or
  `helm.sh/chart` labels, because those would break byte-parity. Helm still
  stamps `app.kubernetes.io/managed-by: Helm` and its `meta.helm.sh/*`
  annotations on the live objects at install time; the rendered manifests
  stay identical to the flattened ones.
- **`HelmLint`** runs `helm lint` with `-n mlobs` and the sentinel values,
  without `--strict`: lint's recommended-label warnings are accepted, for the
  reason above. At the pinned 3.22.0 it raises none for this chart; its one
  finding is `[INFO] Chart.yaml: icon is recommended`.
- **`OperatorE2E`** (`operator/hack/e2e.sh`) installs the operator a second
  time on its k3d cluster, from this chart with `helm install -n mlobs`, and
  runs the same assertions as for the flattened install.

## Helm version

Helm is pinned to 3.22.0, exact, with the release checksum, at the CI pin
site: `HelmParity` and `HelmLint` in `.github/workflows/operator.yml`, and
`OperatorE2E` in `.github/workflows/ci.yml`. The pin is a renderer pin for
byte-parity. Helm 4 is not adopted, and the cost is stated in D38: 3.22 is
the final Helm 3 minor, security-patched only into early 2027, so the pin
outlives this phase by months, not years, and a Helm 4 move is a named
revisit.
