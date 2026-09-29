# operator

The `ServingDeployment` operator — Phase 3 of this repository
(`docs/PHASE3.md`; decisions D31–D37 in `docs/PLAN.md`). One namespaced CRD,
`servingdeployments.serving.mlobs.dev`, and a Go controller for it.

Status: O1. The controller reconciles the stable path. It adopts
`deployment/api` in place — a controller ownerReference added, the name and
pod template kept, only the api image moved to `spec.imageTag` — or creates it
in the `20-api.yaml` shape when it is absent, and records
`status.observedGeneration`. The operator is not deployed anywhere yet; the
canary window lands in O2.

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
