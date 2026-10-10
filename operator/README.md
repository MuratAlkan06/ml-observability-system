# operator

The `ServingDeployment` operator — Phase 3 of this repository
(`docs/PHASE3.md`; decisions D31–D37 in `docs/PLAN.md`). One namespaced CRD,
`servingdeployments.serving.mlobs.dev`, and a Go controller for it.

Status: O2. The operator runs the stable path and the canary window, and
`deploy/k3s/apply.sh` deploys it (`03-operator.yaml`) in D32's order: the CRD
until `Established`, the operator until rolled out, then the
`ServingDeployment` in `40-servingdeployment.yaml`, which renders
`spec.imageTag` and nothing else. The operator adopts `deployment/api` in
place — a controller ownerReference added, the name and pod template kept,
only the api image moved to `spec.imageTag` — or creates it when it is
absent, in the shape pinned by `internal/controller/testdata/api-deployment.yaml`,
and records `status.observedGeneration`. It is the sole writer of three
conditions, each stamped with `lastTransitionTime`: `Ready`, True once
`deployment/api` has rolled out the current spec and is Available (D32's wait
target, with `observedGeneration == generation`); `CanaryActive`; and
`ShadowPaused`, computed from the shadow scorer as observed, never from
intent (D37). It never writes the spec. Leader election is on by default
(`--leader-elect`), through a Lease in `mlobs`; it is tested as the
acquisition of that Lease only, with no failover (D35). The live host moved
to it in O4 (issue #68, completed 2026-10-05); adoption was demonstrated
there on both kinds of object, the original `deployment/api` with its uid
unchanged and a fresh one that the live D36 rollback's pre-operator deploy
created.

## The canary window

A window is opened by a human, by a host-side merge patch that sets both
canary fields; nothing the pipeline renders can open one (D32):

```bash
kubectl -n mlobs patch servingdeployment/api --type merge \
  -p '{"spec":{"canaryImageTag":"<40-hex tag>","canaryReplicas":1}}'
```

It is closed by `apply.sh`'s constant close-window patch — sent on every
pipeline deploy, which prints `ok: canary window closed by the close-window
patch (D32)` when it closes one — or by a human sending the same patch:

```bash
kubectl -n mlobs patch servingdeployment/api --type merge \
  -p '{"spec":{"canaryImageTag":null,"canaryReplicas":0}}'
```

The operator takes one step per reconcile, in the order D26's memory bar
needs on the 4GB host (the D37 addendum of O2):

- **Open:** the shadow scorer is scaled to 0 through its scale subresource and
  observed gone — no pods left — before `deployment/api-canary` is created or
  scaled up at `<prefix>/mlobs-api:<canaryImageTag>`. `CanaryActive` turns True
  on the reconcile that starts the pause.
- **Close or promote:** the canary goes to 0 only once `deployment/api` is
  Available, so a promotion rolls the stable while the canary still serves;
  the canary is observed gone before the shadow scorer returns to 1. The
  canary Deployment stays, at 0, for the next window. A pod counts as gone
  once no container of it runs: one told to stop, whose containers have
  exited, is terminal (Succeeded or Failed) even while its object lingers.
- **TTL:** 45 minutes from `CanaryActive`'s last False→True
  `lastTransitionTime`, kept in status so an operator restart does not reset
  it; a changed `canaryImageTag` mid-window does not restart it. At the
  deadline the operator restores steady and sets `CanaryActive` False with
  reason `WindowExpired`, leaving the canary fields in the spec for the
  close-window patch to clear. Until a reconcile sees the spec cleared, the
  window stays closed whatever those fields say: a new window has to pass
  through the closed shape first (the D32 clarification of O2).

The stack is then in exactly one of two states, and
`deploy/k3s/smoke.sh` judges which from the cluster, never from the spec's
canary fields: steady (`CanaryActive` and `ShadowPaused` False, no canary
pods, the shadow scorer at 1) or window (both True, the shadow scorer at 0,
the canary serving). Anything else, once a transition has had its deadline
to settle, fails it. The canary is scraped as job
`api_canary` through its own Service, `api-canary`.

The operator's whole grant is the one namespaced Role in
`deploy/k3s/manifests/02-operator-rbac.yaml` (D33 and its O2 addendum): its
only write on a StatefulSet is `patch` on the scale subresource of
`shadow-scorer`, and it only ever sends the literal replica counts 0 and 1.

Running a window on the host is covered in
[`docs/RUNBOOK.md`](../docs/RUNBOOK.md): the TTL's arithmetic, the cost of
the stable-first close, and taking the operator back out (D36, rehearsed in
CI by `deploy/k3s/rehearse-rollback.sh`).

## Labels

The stable and the canary share `app: api`; the canary adds `role: canary`.
The `api` Service selects `app: api`, so it reaches both, which is D34's split:
by replica ratio, per connection. The `api-canary` Service selects both labels
and reaches the canary alone.

That leaves the stable Deployment's selector, `app: api`, matching the
canary's pods as well. The overlap is deliberate. The stable's selector is
immutable and its pod template must not change on adoption — a new label
there would restart the live api when O4 adopts it — so the stable keeps the
labels it has always had, and there is no `role: stable`. The Deployment and
ReplicaSet controllers tolerate the overlap: each counts and adopts only the
ReplicaSets and pods whose controller reference is its own or absent, and
every canary pod's points at the canary's ReplicaSet. What does see both is
anything that selects by label alone — `kubectl get pods -l app=api` lists the
canary's pods with the stable's.

## Host requirements

The operator needs nothing from the node, but the `api` Service it serves
behind is a NodePort on 8000, which k3s admits only when started with the D34
pair: `--service-node-port-range=8000-8000` together with
`--disable-network-policy`, never one without the other (the D34 erratum of
O2: k3s's network-policy controller refuses a single-port range and the server
crash-loops). `apply.sh`'s preflight reads the range k3s records and stops
before any change when it does not admit 8000. The pin sites are listed in
`docs/K3S.md`.

## Pins

Scaffolded with kubebuilder v4.15.0 — a scaffold-time tool, not a build
dependency — on controller-runtime v0.24.x and k8s.io v0.36.x, the band that
matches the host's k3s `v1.36.4+k3s1` (D19, D31). envtest runs on 1.36.x
assets. The band moves only together with a k3s bump (D35).

## What was not kept from the scaffold

- The kustomize `config/` tree (D31). The CRD and the Role are flattened into
  `deploy/k3s/manifests/` instead, kept in sync by `make verify-manifests`.
- The kind-based e2e suite under `test/` and its Makefile targets: the
  operator's end-to-end check runs on the pinned k3d image instead (D35), as
  `hack/e2e.sh`.
- The scaffold's own `.github/` workflows, dev container and agent guide.

## Develop

```bash
make manifests  # CRD + Role, spliced into deploy/k3s/manifests/01-, 02-, and the chart's CRD copy
make verify-manifests  # the CI sync check: fails if those three drift from the Go source
hack/helm-parity.sh  # CI's HelmParity: the operator chart against 02- and 03- (Helm 3.22.0)
make generate   # DeepCopy methods (controller-gen object)
make lint       # golangci-lint, custom-built with the logcheck plugin
make test       # envtest: a real kube-apiserver and etcd, 1.36.x
make build      # bin/manager
```

The end-to-end check, as CI's `OperatorE2E` job runs it, from the repository
root. The cluster is started without the D34 pair on purpose: the check's first
assertion is that `apply.sh`'s preflight refuses it. It then installs the
operator twice, from the flattened manifests and then from the chart at
`deploy/helm/mlobs-operator` (D38), so it needs `helm` on `PATH`; CI pins
v3.22.0.

```bash
k3d cluster create mlobs-e2e --image rancher/k3s:v1.36.4-k3s1 \
  --k3s-arg '--disable=traefik@server:*' \
  --k3s-arg '--disable=servicelb@server:*' \
  --k3s-arg '--disable=metrics-server@server:*' --wait
TAG="$(git rev-parse HEAD)"
CANARY_TAG="$(printf '%s' "${TAG}-canary" | shasum | cut -c1-40)"
docker build -t "mlobs-operator:${TAG}" operator
docker build -t mlobs-stub:e2e - < operator/hack/e2e-stub.Dockerfile
docker tag mlobs-stub:e2e "mlobs-api:${TAG}"
docker tag mlobs-stub:e2e "mlobs-api:${CANARY_TAG}"
k3d image import -c mlobs-e2e "mlobs-operator:${TAG}" mlobs-stub:e2e \
  "mlobs-api:${TAG}" "mlobs-api:${CANARY_TAG}"
IMAGE_TAG="${TAG}" CANARY_TAG="${CANARY_TAG}" operator/hack/e2e.sh
k3d cluster delete mlobs-e2e
```
