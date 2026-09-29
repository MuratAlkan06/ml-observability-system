# Phase 2 Task Contract — engineering principles + cloud/DevOps lane

> Companion to the frozen v1 spec in `PLAN.md` (which stays untouched). Governs Phase 2 only. Binding engineering rules for all contributors live in the root `PRINCIPLES.md`.

## Objective

Bind contributors to tailored engineering principles, then close the cloud/DevOps lane with evidence-grade infrastructure: IaC for the certified EC2 shadow-test environment and a managed, CI-deployed runtime.

## Slices

| Slice | Scope | Status |
|---|---|---|
| P0 (#36) | `PRINCIPLES.md` + this contract + CI `DocsGate` + README link. Docs/CI only. | done (#39 merged) |
| P1 (#40) | Terraform codifying the EC2 shadow-test environment: S3 remote state with locking, a single flat root rather than a module tree (D12), `terraform validate` + `plan` in CI. | done (#41 merged) |
| P2 | Compose→k3s migration on the existing EC2 host per stretch-ladder rung 2, including the ladder's required written why/when doc, plus GitHub Actions OIDC deploy pipeline (no long-lived AWS keys). Owner ruling 2026-09-01: k3s — honors the frozen ladder, no deviation entry needed. | done (P2a #50, P2b #55, P2c #57 merged; first gated deploy executed 2026-09-27) |
| P3 (stretch) | Kubernetes manifests/Helm evidence; ephemeral EKS run documented apply → evidence → destroy. | superseded — see erratum below |

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

None. (P2 target resolved 2026-09-01: k3s — see slice table.)

> **Erratum (2026-09-29, P3):** Owner ruling — re-sequenced. A `ModelDeployment` CRD + Go
> controller (kubebuilder) ships before the P3 stretch; P3 is revised to package the operator
> (Helm chart) and demonstrate it via ephemeral EKS, with Compose retirement unchanged at P3
> close. Two ADRs (api-Deployment ownership; monorepo vs separate repo) gate any scaffolding.
> Full contract: `docs/PHASE3.md` (written after the ADRs land).
