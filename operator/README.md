# operator

The `ServingDeployment` operator — Phase 3 of this repository
(`docs/PHASE3.md`; decisions D31–D37 in `docs/PLAN.md`). One namespaced CRD,
`servingdeployments.serving.mlobs.dev`, and a Go controller for it.

Status: O0. The module, the API types and the CRD's validation are in place;
the controller's `Reconcile` is still the scaffold's no-op, and the operator
is not deployed anywhere. Reconciliation lands in O1, the canary window in O2.

## Pins

Scaffolded with kubebuilder v4.15.0 — a scaffold-time tool, not a build
dependency — on controller-runtime v0.24.x and k8s.io v0.36.x, the band that
matches the host's k3s `v1.36.4+k3s1` (D19, D31). envtest runs on 1.36.x
assets. The band moves only together with a k3s bump (D35).

## What was not kept from the scaffold

- The kustomize `config/` tree (D31). The CRD and the Role are flattened into
  `deploy/k3s/manifests/` instead, kept in sync by `make verify-manifests`.
- The kind-based e2e suite under `test/` and its Makefile targets: the
  operator's end-to-end tests run on the pinned k3d image instead (D35).
- The scaffold's own `.github/` workflows, dev container and agent guide.

## Develop

```bash
make manifests  # CRD + Role, spliced into deploy/k3s/manifests/01-, 02-
make verify-manifests  # the CI sync check: fails if those drift from the Go source
make generate   # DeepCopy methods (controller-gen object)
make lint       # golangci-lint, custom-built with the logcheck plugin
make test       # envtest: a real kube-apiserver and etcd, 1.36.x
make build      # bin/manager
```
