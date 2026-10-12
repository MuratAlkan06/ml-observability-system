# Phase 4 Task Contract — metric-driven promote and rollback (operator v1)

> Companion to `PLAN.md`, whose operator-v1 ADR block (D41+) this contract executes once
> frozen; everything frozen there stays append-only. Follows the v0 line drawn in `PHASE3.md`
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
behavior stays v0: a CR that does not opt in is promoted and rolled back by humans.

## Settled constraints

- **Metric split** (carried from `PHASE3.md`). Rollback is decided on health metrics;
  promotion on agreement and confidence against the incumbent stable; drift stays a monitor,
  not a gate.
- **R9 (owner ruling 2026-10-11, recorded on #108).** v4.0.0 requires at least one live
  window where the operator acts autonomously; k3d-only acting proof is not enough for the
  phase's headline claim. Sequence: live shadow-decision window first; the first live
  autonomous act is the safe direction — close/rollback on a deliberately induced breach;
  live auto-promote last, bounded (a same-image promote is an acceptable demonstration).
  Mechanics and abort-handling are set at the design freeze.
- **Shadow-decision first.** A mode in which the operator only records what it would have
  done ships and is exercised — in CI and live — before any slice lets it act.
- **Opt-in.** The decision policy lives in spec and defaults to v0 behavior.
- **Evidence.** When the operator decides, status records why, interval by interval.
- **Deadline.** The D36 way-back's S3 fallback tarballs expire ~2026-11-26/28 (#60, dated
  comment). Live v1 windows are scheduled before that, or the fallback is re-ruled at the
  freeze (R8).

## Slices

Pending the design freeze (D41+); the decomposition below is the candidate set and is
finalized when the freeze lands.

| Slice | Scope | Status |
|---|---|---|
| V1-D | Design freeze: options memo, owner rulings R1–R8 on #108, `PLAN.md` D41+ block, this contract finalized. | in progress |
| V1-S1 | Decision engine as a pure Go package: the O4 rules as code (interval evaluation, warmup, floor, ratio, unevaluable=breach, consecutive-clean counter, breach classification), table-driven tests mirroring the RUNBOOK lines. No controller wiring, no RBAC change. | pending freeze |
| V1-S2 | Metric acquisition + RBAC per R1; manifests, Helm chart and the e2e `can-i` assertions kept in sync; the grant justified verb-by-verb in PLAN (D33 discipline). | pending freeze |
| V1-S3 | Shadow-decision mode: opt-in observe mode records would-have verdicts in status and events and acts on nothing; the e2e stub gains controllable metrics; k3d scenarios: clean pass, latency breach, unevaluable intervals, warmup exclusion. | pending freeze |
| V1-S4 | Acting rollback: on a rollback-classified breach the operator restores steady by the TTL-expiry mechanism (no spec write) and latches with a breach reason; TTL/latch semantics per R7. | pending freeze |
| V1-S5 | Acting promote, per the R3 ruling. | pending freeze |
| V1-S6 | Live validation per R9, plus the runbook/README v1 line: what the operator now decides and what stays human, written as crisply as v0's line. | pending freeze |

## Acceptance criteria

Set at the design freeze, slice by slice. The phase-level bars are fixed now:

- Every new operator behavior has a k3d heavy covering it (OperatorE2E scenarios on the
  controllable-metrics stub; K3sSmoke and the D36 rehearsal stay green throughout).
- The live host is touched only after CI-level proof and an owner go recorded on the slice
  issue first.
- R9's three live demonstrations, evidenced on their issues in the #68/#92 pattern.

## Phase-close criteria

- Tag `v4.0.0` + release notes.
- Every #60 item this phase adds or touches is triaged: resolved or explicitly deferred.
- Every claim the phase adds ships with its evidence (`PRINCIPLES.md` §6).
- The v0→v1 line updated in README and RUNBOOK: what the operator now decides, what stays
  human.

## Verification

Independent verification per slice; skeptical release gate at phase close. Evidence lives in
PR test plans, the slice issues and docs.

## Open questions

The design freeze (V1-D) must answer, owner-ruled on #108 before D41+ lands: R1 the
metric source and the exact RBAC grant; R2 the CRD opt-in shape and the status evidence
record; R3 the promote mechanics under D32's writer set; R4 the promote threshold N
(candidate: the 11 consecutive clean intervals of the live O4 pass); R5 the failure posture
(unevaluable intervals, total metric outage, operator restart mid-window); R6 what stays
human; R7 TTL and latch interaction; R8 the D36 fallback deadline.
