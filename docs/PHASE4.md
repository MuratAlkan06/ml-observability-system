# Phase 4 Task Contract — metric-driven promote and rollback (operator v1)

> Companion to `PLAN.md`, whose operator-v1 ADR block (D41–D50) this contract executes;
> everything frozen there stays append-only. Follows the v0 line drawn in `PHASE3.md`
> ("v1 drives those calls from metrics"). Binding engineering rules for all contributors live
> in the root `PRINCIPLES.md`.

## Objective

v0 (D31–D37) made the canary window mechanical and left promote and rollback as human calls.
v1 mechanizes Gate 4: the operator watches the same signals as the O4 abort-criteria section
of `docs/RUNBOOK.md` and, when the CR opts in, (a) closes the window on the breaches the
classification table assigns to the canary or the host, and (b) records or executes promotion
after N consecutive clean intervals. v1 invents no policy: the O4 rules — 60 s intervals, the
50-requests-per-pod floor, the 1.25× same-interval p95 ratio, the 60 s warmup,
an-unevaluable-interval-counts-as-a-breach, and the classification table — are the frozen
decision rule, negotiated through three review rounds and exercised live at O4 (#68). Default
behavior stays v0: a CR that does not opt in is promoted and rolled back by humans. The freeze
draws the line exactly: the operator acts only on what a pod shows it — lines 1–3 and the
canary's own OOM kill — while the host regime, the window state, the operator itself and the
deploy gate stay human (D46); N is 11, promote-side policy ratified beside the table from the
#68 evidence rather than transcribed from it (D44).

## Settled constraints

- **Metric split** (carried from `PHASE3.md`). Rollback is decided on health metrics;
  promotion on agreement and confidence against the incumbent stable; drift stays a monitor,
  not a gate. D49 re-rules the promotion half on the record: v1 promotes on the O4 health
  table alone, and agreement and confidence stay a human judgement until predictions carry
  per-arm attribution.
- **R9 (owner ruling 2026-10-11, recorded on #108).** v4.0.0 requires at least one live
  window where the operator acts autonomously; k3d-only acting proof is not enough for the
  phase's headline claim. Sequence: live shadow-decision window first; the first live
  autonomous act is the safe direction — close/rollback on a deliberately induced breach;
  live auto-promote last, bounded (a same-image promote is an acceptable demonstration).
  Mechanics and abort-handling are set at the design freeze.
- **Design frozen.** The decisions are D41–D50 in `PLAN.md`'s operator-v1 block. The owner
  rulings are recorded on #108: R9 on 2026-10-11; R1–R8, R10 and R11 on 2026-10-12, all as
  recommended, with the addendum that changing N later requires a new D-number.
- **Shadow-decision first.** A mode in which the operator only records what it would have
  done ships and is exercised — in CI and live — before any slice lets it act.
- **Opt-in.** The decision policy lives in spec and defaults to v0 behavior.
- **Evidence.** When the operator decides, status records why, interval by interval.
- **Deadline.** The D36 way-back's S3 fallback tarballs expire ~2026-11-26/28 (#60, dated
  comment). Live v1 windows are scheduled before that, or the fallback is re-ruled at the
  freeze (R8). Ruled (D48): every live v1 window before 2026-11-20; past the horizon, D36
  step 5's target is a reviewed revert-at-need through the pipeline, its CI-publish
  dependency named.

## Slices

Finalized at the design freeze; each slice names the D41–D50 decisions it executes.

| Slice | Scope | Status |
|---|---|---|
| V1-D | Design freeze: the options memo and an independent risk critique, both on #108; owner rulings R9 (2026-10-11) and R1–R8, R10, R11 (2026-10-12) on #108; the `PLAN.md` D41–D50 block; this contract finalized. | done (rulings on #108; this slice) |
| V1-S1 | Decision engine as a pure Go package, the O4 table transcribed: interval evaluation over per-pod samples, the 50-request floor, the bucket-delta p95 and the 1.25× same-interval ratio, warmup from canary Ready per restart, the immediate trips (post-warmup canary 5xx, canary OOMKilled), unevaluable = breach toward the two-in-a-row counter; the verdict and reason vocabulary including pending, warmup, unevaluable and `OperatorInterrupted`; baseline seeding before the first judged interval; the consecutive-clean counter with N = 11 compiled and the `cleanRun` reset on a canary restart. Table-driven tests with no clock, mirroring `watch-window.sh`'s classification line for line. No controller wiring, no RBAC change (D44, D45). | done (#112 merged) |
| V1-S2 | Metric acquisition and its grant: `pods` get and list in `mlobs`, no watch, read through the uncached APIReader at interval boundaries; per-pod `/metrics` reads at pod IP:8000 under a context timeout shorter than the 60 s grid; the generated RBAC, `OperatorManifestSync`, `HelmParity` and the e2e `can-i` assertions (`operator/hack/e2e.sh`) updated together; the verbs justified in `PLAN.md`, D33 discipline (D41). | done (#114 merged) |
| V1-S3 | Observe mode: `spec.gate` with the ordered mode enum, enum-validated verdict and reason vocabularies and Helm `crds/` parity; tag-bound arming (`spec.gate.forCanaryImageTag`, a mismatch degrading to Observe with an Event); the 50-slot evidence ring and counters in `status.gate`, byte-deterministic between boundaries; Events on transitions only; acts on nothing. The e2e stub gains controllable `/metrics` shaping histogram bucket deltas; k3d scenarios: clean pass, latency breach, unevaluable, warmup/pending; plus the assertions that a pipeline deploy leaves `spec.gate` intact and that the rendered CR manifest has no gate key (D42, D45). | done (#118 merged) |
| V1-S4 | ActRollback: on a breach, steady restored through the TTL-expiry mechanism with no spec write; `CanaryActive` False with reason `BreachRolledBack`, the message naming the tripped line; the shared latch, released only through the closed spec shape (`decideWindow` gains one latched-reason check); the TTL unchanged; the promote verdict recorded, never dispatched; a k3d breach scenario (D45, D47). | done (#120 merged) |
| V1-S5 | ActPromote: latch-then-dispatch of `deploy.yml`, at most once per latched verdict, the tag validated 40-hex; the 15-minute dispatch floor; the queued-run defer; a non-2xx left latched with no retry; the token Secret created host-side and mounted by the operator Deployment only, secret-absent degrading to verdict-only; the machine account and its mint-time scope check; the runbook's rotation/revocation line and the `main`-ancestry precondition for a window opened under ActPromote. Independent security review required (D43, D47). | done (#122 merged) |
| V1-S6 | Live legs 1–3 per D50, all before 2026-11-20 (D48); the runbook v1 line — the acting-window abort checklist gains reject-the-pending-dispatch, a human attends every acting window with `watch-window.sh` beside it, what stays human per D46, the post-expiry rule (D47), no operator-image roll during an acting window (D45), D36 step 5's post-horizon target (D48); README's v0→v1 line updated with the D49 claim language (D46–D50). | done (#124 docs; legs 1–3 evidenced on #123 — leg 1 ruled satisfied 2026-10-14, legs 2–3 2026-10-15; close-out (#127)) |
| V1-S7 | Hotfix, found by the release gate on 24e2b35 (CI run 38790492622, OperatorE2E): the acting-level demotion race — a reconcile served a stale ServingDeployment re-opened a window armed in ActRollback, moving its grid's anchor; another wrote the conditions back over the move; and the gate re-armed the window at Observe. Every status write a reconcile makes is now optimistically locked to the object it read (#121's F4 discipline; the D43 clarification of 2026-10-15), an envtest regression red on 24e2b35 and green with the lock; `operator/hack/e2e.sh` step 8c's failure names `status.gate.mode`, the last record's `effectiveLevel` and any `GateDegraded` Event (D42, D43, D47). | done (#130 merged; verification and security records on #129) |

## Acceptance criteria

Phase-level bars, for every slice:

- Every new operator behavior has a k3d heavy covering it (OperatorE2E scenarios on the
  controllable-metrics stub; K3sSmoke and the D36 rehearsal stay green throughout).
- The live host is touched only after CI-level proof and an owner go recorded on the slice
  issue first.
- R9's three live demonstrations, evidenced on their issues in the #68/#92 pattern.

Per slice:

- **V1-D:** `docs/PLAN.md` gains the D41–D50 block after its last line, and its diff is pure
  addition; the ten decisions carry the owner rulings of 2026-10-12, the N addendum in D44 and
  the sanctioned claim language verbatim in D49; this contract finalized, its open questions
  closed; `DocsGate` green.
- **V1-S1:** the engine is a pure package with no controller wiring and no clock, and the slice
  has no diff under `deploy/k3s/manifests/` or `deploy/helm/`. Its table-driven tests, green in
  `GoTest`, cover row by row: 49 against 50 requests on a pod (unevaluable against evaluable); a
  p95 ratio either side of 1.25×, computed from bucket deltas through `histogram_quantile`'s
  interpolation; an interval starting 59 s against 60 s after canary Ready (warmup against
  post-warmup), applied again after a restart; an interval ending before any canary Ready
  (pending, toward neither counter); each unevaluable cause — a failed read, a pod under the
  floor, a pod identity change, a counter reset — toward the two-in-a-row counter, and two in a
  row tripping; one post-warmup canary 5xx and a canary OOMKilled each tripping at once; the
  first post-warmup interval after baseline seeding carrying a real delta and no free breach; an
  `OperatorInterrupted` interval counted as unevaluable; `cleanRun` reaching the promote verdict
  at exactly 11, not at 10, and resetting on a canary restart. The classification matches
  `watch-window.sh`'s line for line. `GoLint` green.
- **V1-S2:** the Role's one change is `pods` get and list in `mlobs` — no watch, no
  `pods/proxy`, still no ClusterRole — in the generated RBAC, with `OperatorManifestSync` and
  `HelmParity` green; `operator/hack/e2e.sh`'s `can-i` assertions gain get and list on pods
  allowed, and watch on pods and get on `pods/proxy` denied; the pods read goes through the
  uncached APIReader, which the e2e proves under a Role with no watch verb; a `/metrics` read
  against a stub that never answers ends at its context timeout, under 60 s, as an unevaluable
  interval with the reconcile unblocked (test); `OperatorE2E`, `K3sSmoke` and
  `RollbackRehearsal` green.
- **V1-S3:** `spec.gate.mode` is an OpenAPI enum of exactly `Observe`, `ActRollback` and
  `ActPromote`, and an out-of-enum value is rejected by the API server (envtest); the verdict
  and reason fields are enum-validated; the chart's `crds/` copy is byte-exact under
  `OperatorManifestSync`. A CR with no `spec.gate` behaves as v0, the existing e2e unchanged and
  green. In Observe the operator writes per-interval records to `status.gate`, emits Events on
  transitions only, and acts on nothing: through a breach the canary keeps serving and
  `CanaryActive` stays True until the TTL or a close. An acting mode whose
  `spec.gate.forCanaryImageTag` differs from `spec.canaryImageTag` runs as Observe and emits an
  Event (envtest). The ring holds at most 50 records, checked past 50 intervals. Status is
  byte-identical across a reconcile between boundaries — no ring append, no timestamp change —
  pinned by a test. envtest with an injected now covers the counters and the
  `OperatorInterrupted` forfeiture on an operator restart, with `containerStatuses` written
  directly. On the controllable-metrics stub, which shapes histogram bucket deltas, k3d
  scenarios record the expected verdict sequence in `status.gate` for a clean pass, a latency
  breach, unevaluable intervals and warmup/pending; at most one runs at the real 60 s interval,
  and any test-only interval knob is absent from the manifests and the chart, with an assertion
  proving it. CI asserts that a pipeline deploy leaves `spec.gate` intact and that the rendered
  CR manifest contains no gate key.
- **V1-S4:** in ActRollback each trip — two in a row on line 3's counter, where a latency breach
  and an unevaluable interval count alike, or at once on a post-warmup canary 5xx or a canary
  OOMKilled — restores steady with no spec write, the canary fields left as they were (the D32
  clarification), and sets `CanaryActive` False with reason `BreachRolledBack`, its message
  naming the tripped line and the evidence in `status.gate`; two consecutive
  `OperatorInterrupted` intervals roll back the same way; editing the canary fields does not
  reopen the window, and only the closed spec shape releases the latch (envtest, injected now);
  `WindowExpired` still fires at 45 minutes in every mode (envtest); the promote verdict is
  recorded and nothing is dispatched; a k3d breach scenario on the stub ends with
  `BreachRolledBack` on the CR and the window closed.
- **V1-S5:** an independent security review recorded on the slice issue before merge. The
  token's scope recorded on the slice issue at mint time — Actions read/write on this repository
  only, no Deployments permission, held by the machine account as a Write collaborator — with
  its expiry bounded to the live-window season; D43's residual capabilities restated in the
  runbook. Against a stubbed GitHub API (test): the verdict is written to status before the
  single `workflow_dispatch` of `deploy.yml`, whose input is the 40-hex-validated
  `spec.canaryImageTag`; a re-reconcile of a latched verdict does not dispatch again; a non-2xx
  keeps the verdict latched, emits an Event and is not retried; a queued or waiting Deploy run
  defers the dispatch, the verdict latched and an Event emitted; a remaining TTL under 15
  minutes records the verdict without dispatching; a tag that is not 40-hex is never dispatched;
  with the Secret absent, ActPromote records the verdict only. The Secret is mounted by the
  operator Deployment alone, and the Role is unchanged by this slice (the sync check). The
  runbook gains the token's rotation and revocation line and the precondition that a window
  opened under ActPromote names a canary tag that is a commit on `main`.
- **V1-S6:** each leg opens only after an owner go recorded on the slice issue, and all three
  run before 2026-11-20 (D48). Leg 1 (Observe): the session-level agreement rule declared on the
  issue before the window opens, `watch-window.sh` beside the operator, and the result — the
  same trip or no-trip per line and the same session breach counts — recorded with the
  `status.gate` record. Leg 2 (ActRollback): the breach induced by fresh-connection load on the
  canary pod alone, or by the `kubectl delete pod` fallback, the method used recorded;
  `CanaryActive` False with `BreachRolledBack` naming the tripped line; `deploy/k3s/smoke.sh`
  ending `ok: smoke passed (steady state)`; the line-5 watch kept throughout. Leg 3
  (ActPromote): same-image, on a freshly deployed tip SHA; the reviewer's lift and its restore
  each recorded on the issue with its time; the dispatched run's URL recorded; the close patch's
  fixed line and green smoke in that run; then the PAT expired. `docs/RUNBOOK.md` gains the v1
  line: "check Actions for a pending operator dispatch and REJECT it" in the acting-window abort
  checklist; a human attends every acting window, with `watch-window.sh` beside it; what stays
  human, per D46; the post-expiry rule (D47); the no-operator-image-roll line (D45); D36 step
  5's post-horizon target and its CI dependency (D48). README's v0→v1 line carries the D49 claim
  language verbatim, landed only once all three legs are evidenced.

## Phase-close criteria

- Tag `v4.0.0` + release notes — pending the release gate (GO) and the gated tag window;
  release notes as the GitHub release body (the v3.0.0 precedent).
- Every #60 item this phase adds or touches is triaged: resolved or explicitly deferred — triaged
  on #60 (comment of 2026-10-15: one line executed, four deferrals with triggers, one option
  struck, the sittings' access key to be deleted after GO).
- Every claim the phase adds ships with its evidence (`PRINCIPLES.md` §6) — #123 (the three
  legs, rings attached), #121 (the App record), #108 (rulings and obligations); the README
  claim landed in #127.
- The v0→v1 line updated in README and RUNBOOK: what the operator now decides, what stays
  human — #124 (RUNBOOK v1 section, README v0→v1) and #127 (the sanctioned D49 claim with its
  evidence).

## Verification

Independent verification per slice; skeptical release gate at phase close. Evidence lives in
PR test plans, the slice issues and docs.

## Open questions

None. All design rulings are recorded on #108: R9 (2026-10-11); R1–R8, R10, R11 (2026-10-12).
The freeze is D41–D50.
