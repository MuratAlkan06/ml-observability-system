# Coding Principles — ml-observability-system

These rules bind every contributor to this repository, human or agent. The repo is public resume evidence: strangers will read it against every claim in its README, and its worth is that stated claims and built reality never diverge. When a rule conflicts with cleverness, the rule wins. Exceptions are documented in `docs/PLAN.md` (erratum or D-number) in the same commit. The frozen v1 spec in `docs/PLAN.md` is append-only: errata, never edits.

## 1. Naming and the three domain laws

**Law 1 — model identity is three different things.**
- `model_name` — the Hugging Face repo id (`distilbert/distilbert-base-uncased-finetuned-sst-2-english`).
- `model_revision` — the pinned HF commit SHA.
- `model_version` — the public frozen string echoed in responses, stream events, and DB rows (`distilbert-sst2-v1`, `minilm-sst2-v1`).

Never conflate them. There is no `model_id` in this codebase; introducing one is a defect (CI-enforced).

**Law 2 — the model pair is `primary` / `shadow`.** Columns and metric labels use `primary_*` / `shadow_*`; "candidate" is acceptable in prose. The champion/challenger vocabulary is banned (CI-enforced). Metrics separate models by Prometheus `job` label (`job="drift"` vs `job="drift_shadow"`), never by a metric label; only the comparison matrix uses `{primary_label, shadow_label}`.

**Law 3 — time carries its unit and its clock.** Wire latency and timestamps are `*_ms` (epoch milliseconds: `ts_ms`); Postgres stores tz-aware UTC (`ts TIMESTAMPTZ`); everything Prometheus is seconds (`*_seconds`; histograms in seconds — the API's `latency_ms` response field is client convenience only). Scheduling/elapsed time uses `time.monotonic()`; latency measurement uses `time.perf_counter()`; wall-clock is always `timezone.utc`-aware. A bare `timeout`, `interval`, or unitless duration name is a defect in new code.

Supporting rules:

- Scalars carry units: `_ms`, `_seconds`, `_nats`, `_ratio`, `_entries`. Constants take the full suffix — `EVAL_INTERVAL_SECONDS`, not `_S`. (`RECOVER_INTERVAL_S`-style names in `src/consumer/consumer.py` and `src/shadow_scorer/scorer.py` are grandfathered: fix on touch, own commit.)
- Prometheus: metric prefix `mlobs_<service>_`; counters are declared without `_total` (the client appends it) — counters in `src/consumer/metrics.py` and `src/drift/metrics.py` declare the suffix explicitly and are grandfathered (the client normalizes both forms to the same exposed series); new counters use the unsuffixed form; every label set is pre-initialized at import so all series exist from the first scrape; label values come from frozen enums (`test ∈ {class, token_length, confidence}`; `outcome ∈ {evaluated, skipped_insufficient_samples, error}`).
- No `src/common`, zero cross-service imports. A needed copy is duplicated consciously with a comment naming the original (pattern: `src/shadow_scorer/parsing.py`). The event contract lives in `src/consumer/parsing.py`, `src/shadow_scorer/parsing.py`, and the DDL in `tests/drift/conftest.py`; touch one → re-verify all three against `sql/init.sql` in the same commit.
- New env vars take the `MLOBS_<SERVICE>_` prefix (pattern: `MLOBS_SHADOW_MODEL_VERSION`). Existing unprefixed vars are grandfathered until a dedicated migration slice; add no new ones.

## 2. Settings, constants, seams

- Deployment-varying values live in the service's `config.py` (`pydantic-settings`); PLAN-frozen values live in `constants.py` and are never made configurable (`src/drift/constants.py` docstring; enforced by `tests/test_scaffold.py`). `os.environ[...]` outside a settings class is a defect in new code.
- Dependencies enter through seams: `create_app(model_loader=…, redis_factory=…)`, `SlackAlerter(clock=…, post=…)`. Reaching for `time.time()` or a real HTTP client where a seam exists is rejected in review.
- Async exists only at the API edge; consumer, shadow scorer, drift runner, and simulator are blocking loops with SIGTERM/SIGINT → `stop()`. A new async service requires a D-number decision first.

## 3. Error handling

- Custom errors subclass `ValueError` and live beside their raiser (`MalformedEntry` in each `parsing.py`; `BaselineValidationError` in `src/drift/baseline.py`). No shared exceptions module.
- Boundary idiom: validate inside one `try`; `except MalformedEntry: raise`; then `except (KeyError, ValueError, TypeError) as exc: raise MalformedEntry(str(exc)) from exc`.
- `except Exception` is legal only at a documented boundary (fire-and-forget XADD, the API 500 handler, the never-crash drift loop, per-event shadow scoring), annotated at the site — and it must leave a trace: a counter or a log line. `PredictionProducer.ping()`'s silent `False` and the DEBUG-only gauge-refresh log are named debt; do not add a third.
- Secrets never reach logs: the Slack failure path logs without the exception message because httpx embeds the webhook URL. Apply the same care to any future credentialed client.
- The API's 500 body is `{"detail": "internal_error"}`; no stack traces cross the boundary.

## 4. The delivery law (pipeline state)

At-least-once in, exactly-once effect out: `XREADGROUP` → validate → one transaction `INSERT … ON CONFLICT (request_id) DO NOTHING` → `XACK` only after COMMIT. The idempotency key is the server-generated UUID4 `request_id`, `UNIQUE` in Postgres. Redelivery is observable, not merely tolerated (`mlobs_consumer_duplicates_skipped_total` from `len(records) - cur.rowcount`). Recovery: `XAUTOCLAIM` at startup and every 60 s; poison entries drop after more than 5 deliveries. New consumers replicate this shape or record a D-number explaining why not. Schema changes are never hand-patched onto a live volume.

## 5. Testing

- Test names encode behavior and the frozen constant asserted: `test_cooldown_suppresses_within_900s_and_rearms_after`, `test_same_event_twice_produces_one_row`. A name that merely restates a method name is a smell.
- Time is injected (`FakeClock`; `clock=` seams). Asserting on real durations is banned; the only sanctioned sleep is a bounded readiness poll (small step inside an explicit deadline).
- Integration tests may skip locally but must not vanish in CI: the `skip_or_fail()` + `REQUIRE_<SUITE>=1` idiom (`tests/drift/conftest.py`). Every new integration suite ships its `REQUIRE_` CI job in the same slice. (The consumer integration suite's silent skip in CI is named debt.)
- Test the real thing when it is the thing under test: throwaway `postgres:16-alpine` for SQL paths; fakes only through seams built for them.
- Every fixed bug gets a regression test in the same commit; a justified omission is disclosed in the PR and a PLAN erratum, never silent.

## 6. Honesty and documentation

- A performance number exists only with its methodology — host, tool + version, warm-up, measurement window, reproduce block (the README `hey` sections are the template). EC2 numbers and local M4 numbers never mix.
- Overclaim vocabulary is banned in `README.md`, `docs/`, `src/`, `tests/`: "champion"/"challenger", "distributed", "production-ready", "production-grade". In the README, bare "exactly-once" without "effect" is banned — only the exactly-once *effect* is true. CI enforces both sweeps (this file and the workflow file are excluded; frozen PLAN text and in-code docstrings are review-enforced). Do not test the gate.
- Known approximations and tradeoffs are first-class content (token-length chi² binning; restart re-alert; no-ground-truth agreement caveat). New ones follow suit.
- The decision log is `docs/PLAN.md`: append-only errata (`> **Erratum (<date>, <slice>):** …`) and D-numbered decisions cited from code (`v1.1 D5`). Non-obvious decisions get a D-number in the same commit.
- Docs ride in the slice: behavior and its description change in one diff.
- No AI-attribution trailers in commits, PR bodies, or releases (CI sweeps PR commit messages).

## 7. Change discipline

- Smallest correct diff. No drive-by refactors; grandfathered-debt fixes happen on touch only and as their own commit.
- Conventional commits; issue → branch → draft PR → green CI → squash merge; phase tags only at phase boundaries.
- Frozen artifacts are append-only; published tags and cited SHAs never move.

## 8. Dependencies

- Per-service `requirements/*.txt` is the authoritative pin source (PLAN erratum 2026-07-23); Dependabot is the authoritative bumper. `~=` patch-compatible by default; `==` only under a documented ecosystem constraint.
- torch / transformers / tokenizers bump only together; the rule stays written at the pin sites.
- The drift service stays stdlib-only for math; adding numpy/scipy there is a D-number decision, not a convenience.
- Any new dependency requires a PLAN erratum or D-number justification in the same commit.

## 9. Enforcement map

| Rule | Enforced by |
|---|---|
| No `model_id` in src; no champion/challenger, "distributed", "production-ready/-grade"; README bare "exactly-once" (§1, §6) | CI `DocsGate` grep sweeps |
| No AI-attribution trailers (§6) | CI `DocsGate` PR commit-message sweep |
| README links this file | CI `DocsGate` |
| Frozen constants + PLAN Appendix intact (§2) | `tests/test_scaffold.py` (existing) |
| Lint | CI `ruff check .` (defaults today; ruleset expansion tracked in #38) |
| Integration suites cannot silently skip (§5) | `REQUIRE_DRIFT_INTEGRATION=1` CI job (existing); new suites add theirs |
| Image size bound; label-map sanity | CI `DockerBuild` / `ShadowBuild` (existing) |
| Everything else | review → independent verification → release gate |
