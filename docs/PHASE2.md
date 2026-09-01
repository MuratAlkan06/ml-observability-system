# Phase 2 Task Contract — engineering principles + cloud/DevOps lane

> Companion to the frozen v1 spec in `PLAN.md` (which stays untouched). Governs Phase 2 only. Binding engineering rules for all contributors live in the root `PRINCIPLES.md`.

## Objective

Bind contributors to tailored engineering principles, then close the cloud/DevOps lane with evidence-grade infrastructure: IaC for the certified EC2 shadow-test environment and a managed, CI-deployed runtime.

## Slices

| Slice | Scope | Status |
|---|---|---|
| P0 (#36) | `PRINCIPLES.md` + this contract + CI `DocsGate` + README link. Docs/CI only. | in flight |
| P1 | Terraform codifying the EC2 shadow-test environment: S3 remote state with locking, clean module structure, `terraform validate` + `plan` in CI. | queued |
| P2 | Managed deployment + GitHub Actions OIDC deploy (no long-lived AWS keys). Target open: ECS Fargate vs the stretch ladder's k3s — owner ruling pending; deviating from the ladder records a D-number. | blocked on ruling |
| P3 (stretch) | Kubernetes manifests/Helm evidence; ephemeral EKS run documented apply → evidence → destroy. | queued |

## Out of scope

Application/model feature changes; MLflow; auth/multi-tenancy; always-on cloud infrastructure.

## Constraints

Conventional commits; issue → branch → draft PR → green CI → squash merge; no AI-attribution trailers; phase tag only at phase close; every README claim ships with its evidence; infra runs are ephemeral with a recorded cost note.

## Acceptance criteria

- **P0:** `PRINCIPLES.md` at root with domain laws + enforcement map; `DocsGate` green; README links it.
- **P1:** `terraform validate` + `plan` green in CI against a documented backend; docs updated in-slice.
- **P2:** one-command deploy from CI via OIDC; documented rollback; post-deploy smoke check green.
- **Phase close:** tag + release notes; branch protection (#37) resolved or deferred with a D-number.

## Verification

Independent verification per slice; skeptical release gate at phase close. Evidence lives in PR test plans and docs.

## Open questions

1. P2 deployment target (ECS Fargate vs k3s) — owner ruling pending.
