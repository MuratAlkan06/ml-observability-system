# Phase 3 Task Contract — ServingDeployment operator + revised P3 stretch

> Companion to `PLAN.md`, whose operator-phase ADRs (D31–D37) this contract executes; everything frozen there stays append-only. Supersedes the forward pointer in `PHASE2.md`'s P3 erratum. Binding engineering rules for all contributors live in the root `PRINCIPLES.md`.

## Objective

Ship a `ServingDeployment` operator — one namespaced CRD and a Go controller at `/operator` — before the P3 stretch, per the owner's 2026-09-29 re-sequencing ruling. It automates the promote-or-hold decision that v1.1 left as a written judgement, in two stages: v0 makes the canary window mechanical — opened by a human, closed by any pipeline deploy, its state enforced and checked — while the promote and rollback calls stay human (D32, D34, D37); v1 drives those calls from metrics. The revised P3 then packages the operator as a Helm chart, demonstrates it on an ephemeral EKS cluster (apply → evidence → destroy), and retires Compose at its close (D27).

## Settled constraints

From the owner brief (#62).

- **Metric split.** Rollback is decided on health metrics; promotion on agreement and confidence against the incumbent stable; drift is excluded as a canary gate. The drift job reads the latest 500 rows for one `model_version` (`PLAN.md` §5) and the predictions table has no pod or image column, so stable and canary rows are indistinguishable in its window — and a shift in the live traffic trips it for both arms at once. Drift stays a monitor, not a gate.
- **Two stages.** v0: manual promote, human-triggered rollback. v1: metric-driven, on the split above. O0–O4 deliver v0; no slice below scopes v1.
- **Budget.** 4–6 weekends.
- **Leader election.** kubebuilder's default leader election, demonstrated. The brief named kind; D35 runs it on the pinned k3d image instead and scopes it to a lease-acquisition assertion.
- **Why not Argo or KServe.** A written artifact explaining why this phase builds its own operator rather than adopting either is a deliverable of the phase.

## Slices

| Slice | Scope | Status |
|---|---|---|
| O0 (#64) | `/operator` Go module (kubebuilder v4.15.0 scaffold; controller-runtime v0.24.x, k8s.io v0.36.x); `ServingDeployment` CRD types; hand-flattened manifests + controller-gen sync check, kustomize tree deleted; envtest 1.36.x; `GoLint`/`GoTest` CI; `^operator/` in `K3sPaths`; `DocsGate` over `operator/`; Dependabot `gomod` band (D31, D33, D35). | done (#70 merged) |
| O1 (#65) | Reconcile core: in-place ownerReference adoption of `deployment/api`, name kept; stable-tag reconcile from `spec.imageTag`; `CanaryActive`/`ShadowPaused` conditions with `lastTransitionTime`, operator the sole writer; leader election (D31, D37). | done (#79 merged) |
| O2 (#66) | Canary Deployment, `api-canary` Service and `api_canary` scrape job; NodePort 8000 on the exact 8000-8000 range with `externalTrafficPolicy: Local`, the flag in all three pin sites, and the D19 erratum; shadow-pause protocol; `20-api.yaml` loses its Deployment; the `apply.sh` D32 sequence with the close-window patch; state-aware `smoke.sh` (D32, D34, D37). | done (#81 merged) |
| O3 (#67) | D36 rollback across the boundary rehearsed end-to-end in k3d CI; runbook carrying the 45-min TTL, its 50000 / rps trim math and the steps that run outside the pipeline; the D34 honesty section in README/docs; the why-not-Argo/KServe positioning write-up (owner-brief deliverable) (D34, D36, D37). | done (#84 merged) |
| O4 (#68) | Live EC2 cutover, gated: host NodePort flag + k3s restart; the operator adopts the live api; one live canary window (≤ 45 min TTL) opened, then promoted or closed; the D36 sequence demonstrated live once. | done (#68) |
| H1 | Helm packaging of the operator (D38): the chart at `deploy/helm/mlobs-operator/` with the tag and namespace guards; `HelmParity` and `HelmLint` CI jobs, one negative test per guard; `OperatorManifestSync` extended to the chart's `crds/` copy; `OperatorE2E` gains a chart-install phase; `deploy/helm/` joins `K3sPaths`; Helm pinned at the CI pin site. ≈ 1 weekend. | pending |
| H2 | Ephemeral EKS demonstration (D39): committed `deploy/eks/` config; the owner-run demo script — preflights before cluster create, fixed-line assertions, per-run nonce, scratch kubeconfig, the T+2h window-segment valve and the hard T+3h teardown trap, the pinned orphan sweep; the live run on the designated non-root principal; evidence including the T+24h sweep-and-billing line and the `infra/ec2` plan exit-0 backstop; Helm and eksctl pinned at the script pin site. ≈ 1 weekend. | pending |
| H3 | Compose retirement (D40): the enumerated textual sweep with the last shipping SHA recorded; phase close — every #60 item triaged, release notes, tag `v3.0.0`. ≈ 0.5 weekend. | pending |

## Acceptance criteria

- **O0:** envtest boots on the pinned 1.36.x assets; the controller-gen sync check green; no diff under `infra/ec2/`; `DocsGate` green with `operator/` swept.
- **O1 (envtest):** adoption preserves the api's name and pod template; status reaches `observedGeneration == generation`; lease acquisition asserted, with no timing-based failover; every condition transition carries `lastTransitionTime`.
- **O2 (k3d, stub image):** window open → `CanaryActive` and shadow at 0, smoke green; window closed → steady, smoke green; a deploy during an open window closes it, on a fixed line; `127.0.0.1:8000` reachable; a constructed third state rejected by smoke.
- **O3:** the D36 rehearsal job green in CI; docs gates green; the why-not write-up merged.
- **O4:** before live traffic — a `pg_dump` preflight; a measured window rehearsal (two api pods, shadow at 0, 5 rps; `MemAvailable` ≥ 400MB, zero OOM kills); written abort criteria; no EBS snapshot, ruled out explicitly with the reasoning recorded against D30. Then: state-aware smoke green on the host in both states; evidence on #68.
- **H1:** `HelmParity` green: the chart rendered with `helm template -n mlobs` at the sentinel
  prefix and a 40-hex sentinel tag diffs empty against the flattened operator manifests with
  `IMAGE_PREFIX`/`IMAGE_TAG` sed-rendered to the same sentinels, exact after stripping only
  `# Source:` lines — byte-identical, no Helm-added labels, `helm lint`'s recommended-label
  warnings accepted and stated; each guard fails its negative test on a fixed line (a
  non-40-hex `image.tag`; a release namespace other than `mlobs`); `OperatorManifestSync`
  green over three projections, the chart's `crds/` copy byte-exact whole-file; `OperatorE2E`'s
  chart-install phase green on the pinned k3d image under the pinned Helm 3.22.x; no
  `ServingDeployment` anywhere under `deploy/helm/`; `K3sSmoke` green with the flattened path
  untouched.
- **H2:** the evidence transcript opens with `aws sts get-caller-identity` on a fixed line, on
  the designated non-root admin principal; before cluster create — GHCR manifest preflights
  for the three pulled images (the deploy.yml pattern), `helm` and `eksctl` versions asserted
  on fixed lines, EKS 1.36 availability and the $0.10/h control-plane rate re-verified
  same-day; then, on fixed lines plus kubectl outputs: operator Ready and Lease held;
  `deployment/api` at the rendered image, `Ready` at `observedGeneration == generation`;
  `/health` and `/predict` through port-forwards; window open with `CanaryActive` True at the
  current generation and the pod-scoped canary evidence — port-forward to
  `deployment/api-canary`, `/predict` 200 with `request_id`/`label`/`confidence`, the canary's
  own `mlobs_predictions_total` incremented across the forward; the constant close patch, then
  steady; `ShadowPaused` False/`ShadowNotFound` recorded as expected; the D36-shaped teardown
  with the CRD deleted explicitly last, `eksctl delete cluster --wait` green, the scratch
  kubeconfig deleted; the orphan sweep all-absent on fixed lines, filtered by the three
  cluster-scoped tag families only; `terraform -chdir=infra/ec2 plan` exit 0; the hard T+3h
  teardown-first trap present in the script; the cost line in the frozen form ("3h wall clock
  × verified rates, verified next day"); and the T+24h line — sweep re-run all-absent plus the
  billing check — recorded. The T+2h window-segment valve is either unused or exercised
  exactly as specified.
- **H3:** `docker-compose.yml` absent from the tree, its last shipping SHA recorded; the nine
  provenance headers, `.env.example`, `sql/migrations/002_shadow.sql`,
  `docker/drift.Dockerfile` and the README Reproduce block retargeted or relabelled per D40,
  with measured labels byte-unchanged; README Quick start is the k3d recipe; the
  `deploy/k3s/README.md` source-of-truth paragraph updated and D25's fallback clause closed;
  the no-functional-reference sweep shown in the PR; `DocsGate` green; every #60 item resolved
  or explicitly deferred; release notes published and `v3.0.0` tagged.

## Phase-close criteria

- Tag `v3.0.0` + release notes.
- Every #60 hardening item triaged: resolved in the phase or explicitly deferred.
- Every claim the phase adds ships with its evidence (`PRINCIPLES.md` §6).
- The why-not-Argo/KServe write-up is published (lands in O3).
- Compose retirement executed.

## Verification

Independent verification per slice; skeptical release gate at phase close. Evidence lives in PR test plans, the slice issues and docs.

## Open questions

None. All rulings are recorded: the kind's name (D31) and the traffic-split details (D34), both owner-ruled 2026-09-29.
