# ML Observability System — v1 Plan (FROZEN)

> Frozen v1 specification, transcribed verbatim on 2026-07-14 from the project planning artifact. Single source of truth for all implementation slices; where a value appears here, it is frozen.

## Frozen scope contract (v1)

**IN:**
1. **Inference service:** FastAPI + self-hosted DistilBERT (SST-2 sentiment). `POST /predict`, `GET /health`, `GET /metrics` (prometheus-client), structured logging, typed config.
2. **Event pipeline:** API `XADD`s prediction events to **Redis Streams** → consumer service (consumer group, at-least-once) → **PostgreSQL** predictions table.
3. **Drift detection:** frozen reference baseline vs sliding production window; **Chi-squared** (prediction-class + token-length distributions) + **KL divergence** (confidence distribution); drift scores exported to Prometheus; **Slack webhook alert** on threshold breach.
4. **Traffic simulator with drift injection** (`--mode drift` swaps input domain corpus) — the demo engine; without it the platform demonstrates nothing.
5. **Grafana dashboards** provisioned as JSON in-repo: latency p50/p95, throughput, prediction/confidence distributions, drift scores.
6. **Engineering hygiene:** pytest suite, GitHub Actions CI (lint + tests + docker build), Docker Compose full stack, load-test numbers (hey/locust) recorded in README, mermaid architecture diagram, demo GIF, polished README.
7. **Deploy:** single AWS **EC2 t3.medium** via Docker Compose (stop instance when not demoing); set GitHub repo `homepage` field to demo/README anchor.

**OUT (v1):** Kubernetes, MLflow, retraining pipelines, multiple models, auth/multi-tenancy, any custom frontend (Grafana is the UI).

**STRETCH LADDER (post-v1, strictly in order):** (1) shadow/canary second-model comparison on live traffic; (2) k3s migration **with a written why/when doc**; (3) MLflow only if a fine-tuning component is added.

## Decisions & defaults (user may veto at approval)

- **Delete:** `README.md` (rewrite), `requirements.txt` (rewrite), `src/**` (all empty), `tests/**` (empty), `Dockerfile` (empty), `.dockerignore` (empty), `__pycache__/`, local `venv/` (recreate). **Keep:** `.git` history, `LICENSE`; refresh `.gitignore`.
- **Python 3.12** pinned everywhere (README, `python:3.12-slim` base image, CI matrix); dependency pins refreshed to current-compatible versions — design pass verifies via context7 (old pins like transformers 4.36.2/tokenizers 0.15.0 predate py3.12/3.13 wheel reality; do not carry them forward blindly).
- **Postgres schema via plain SQL init script** (no ORM/Alembic at this scale) — confirm in design pass.
- Repo stays **public from the first commit**; resume bullets and built reality must match at all times.
- **No AI-attribution trailers** in commit messages or PR bodies (`Co-Authored-By: Claude`, `Generated with Claude Code`, etc.) — github-workflow agent enforces; all implementation-slice prompts must state this.
- **github-workflow agent stays on Sonnet** (deliberate exception to the Opus policy): it is checklist-driven and the most frequently invoked agent (gates every mutation), so latency dominates and its explicit rules were written for mechanical enforcement.
- At execution start, read target branch via `sc worktree status --json`; all git/GitHub mutations gated by the **github-workflow** agent (issue → branch → draft PR → conventional commits → squash merge → phase tags `v0.1.0`→`v1.0.0`).

# v1 Design Specification (FROZEN 2026-07-14 — Fable 5 design pass)

Single source of truth for v1. Implementation sessions build against it without asking questions; where a value appears here, it is frozen. Session A transcribes this entire section verbatim into `docs/PLAN.md`. Pins verified against live PyPI JSON metadata on 2026-07-14 (context7 MCP was unavailable; PyPI metadata is the stronger source for pins).

## 0. Task contract
- **Objective:** sentiment inference API (self-hosted DistilBERT SST-2) + event pipeline (Redis Streams → consumer → PostgreSQL) + statistical drift detection (Chi-squared + KL) + Prometheus/Grafana + Slack alerting, driven by a traffic simulator.
- **Compose services:** `api`, `consumer`, `drift`, `redis`, `postgres`, `prometheus`, `grafana`. Simulator runs on the host.
- **Non-goals:** auth, TLS, batch prediction, retraining/registry, ORM/Alembic, Kubernetes, horizontal scaling, multi-model, retention jobs. Grafana dashboard JSON authored by implementation slices against §6 (not frozen here).
- **Constraints:** Python 3.12; `python:3.12-slim`; plain SQL init; single t3.medium (~4GB); CPU inference; 1 uvicorn worker; `torch.set_num_threads(2)`.
- **Assumptions:** ≤ ~20 req/s, thousands of rows/day; single consumer; empty `SLACK_WEBHOOK_URL` silently disables alerting (job still evaluates + records).
- **Acceptance:** `cp .env.example .env` (set `POSTGRES_PASSWORD` + `GF_ADMIN_PASSWORD`) → `docker compose up` → passing `/health`; simulator drift mode fires all three drift tests within 5 min; duplicate stream deliveries produce zero duplicate Postgres rows; all §6 metrics visible in Prometheus.

## 1. Dependency pins (PyPI-verified, cp312 wheels confirmed)
Pin style `~=X.Y.Z` (patch-compatible) except where an ecosystem constraint forces `==`.

```text
# requirements/api.txt
fastapi~=0.139.0
uvicorn[standard]~=0.51.0
pydantic~=2.13.4
pydantic-settings~=2.14.2
transformers==5.13.1          # EXACT: v5 ships breaking changes in minor releases; requires torch>=2.4
tokenizers==0.22.2            # EXACT: transformers 5.13.1 caps <=0.23.0; 0.23.0 final was never published, 0.23.1 violates the cap
torch==2.13.0                 # install from CPU index (below)
prometheus-client~=0.25.0
redis~=8.0.1

# requirements/consumer.txt
redis~=8.0.1
psycopg[binary]~=3.3.4
prometheus-client~=0.25.0
pydantic-settings~=2.14.2

# requirements/drift.txt
psycopg[binary]~=3.3.4
prometheus-client~=0.25.0
pydantic-settings~=2.14.2
httpx~=0.28.1

# requirements/dev.txt
pytest~=9.1.1
httpx~=0.28.1
ruff~=0.15.21
```

> **Erratum (2026-07-14, S1):** `tokenizers==0.23.0` was never published to PyPI (only 0.23.0rc0/0.23.1 exist); transformers 5.13.1 caps `<=0.23.0`, so the highest installable final is `0.22.2`. Pin corrected; coupling rule unchanged.

> **Erratum (2026-07-23):** `fastapi~=0.139.0` and `ruff~=0.15.21` above reflect the 2026-07-14 verification snapshot; both have since moved to `0.139.2` and `0.15.22` respectively via merged Dependabot PRs, within the original `~=` band. requirements/api.txt and requirements/dev.txt are the authoritative, dependabot-maintained pins; this table is not updated in place.

- **torch CPU install (frozen):** api Dockerfile MUST run `pip install torch==2.13.0 --index-url https://download.pytorch.org/whl/cpu` first (default Linux wheel bundles CUDA — blows RAM/disk budget).
- **No scipy/numpy:** drift math implemented in pure Python against hard-coded critical values (§5) — saves ~60MB in slim image; interview talking point.
- **transformers v5 notes:** pass `dtype=torch.float32` explicitly (deterministic CPU); `TextClassificationPipeline` remains supported in v5.
- **The two exact pins (`transformers==5.13.1`, `tokenizers==0.22.2`) are coupled — bump only together.**
- **Docker images:** `python:3.12-slim`, `postgres:16-alpine`, `redis:7.4-alpine` (≥7 required for XINFO GROUPS lag + XAUTOCLAIM), `prom/prometheus:v3.5.0` (LTS), `grafana/grafana:12.1.0`.
- **Model pin:** HF `distilbert-base-uncased-finetuned-sst-2-english`; resolve current commit SHA once at implementation time, hard-code as default `MODEL_REVISION`, bake snapshot into api image (`HF_HOME=/opt/hf-cache`), no runtime downloads. Public string `MODEL_VERSION = "distilbert-sst2-v1"` (frozen) appears in responses, events, DB rows.

## 2. HTTP API contract (service `api`, port 8000)
**Decision — single-text only, no batch:** at demo scale batching adds queueing/latency ambiguity and breaks the 1-request→1-event→1-row invariant that makes idempotency trivial.

### POST /predict
Request: `{"text": string}` — required; ≤1000 chars; ≥1 non-whitespace char (`min_length=1` + `strip() != ""` validator); unknown fields rejected (`extra="forbid"`).

Response 200 (all fields non-nullable):
```json
{"request_id": "<uuid4>", "label": "positive", "confidence": 0.998712,
 "model_version": "distilbert-sst2-v1", "latency_ms": 42.17}
```
- `request_id`: server-generated UUID4 — pipeline-wide idempotency key.
- `label`: `"positive"|"negative"` (model output lowercased).
- `confidence`: max softmax ∈ [0.5, 1.0], 6 decimals.
- `latency_ms`: tokenization + forward pass only (`perf_counter` around pipeline call), 2 decimals.
- Inference config (frozen): `truncation=True, max_length=256`. `token_count` = `len(input_ids)` after truncation **including** [CLS]/[SEP] ⇒ range [3, 256]. Baseline builder MUST use identical tokenization.
- Side effect: on success XADD one event (§3), **fire-and-forget** — XADD failure still returns 200, increments `mlobs_stream_publish_failures_total`, logs ERROR. (Prediction availability beats event completeness; loss is observable.)

### GET /health
`{"status": "ok|degraded|unavailable", "model_loaded": bool, "redis_connected": bool, "model_version": "distilbert-sst2-v1"}`
- model not loaded → 503 `"unavailable"`; model ok + Redis PING fail (250ms timeout) → 200 `"degraded"`; both ok → 200 `"ok"`. Used as Compose healthcheck.

### GET /metrics
`prometheus_client` text exposition; default process/platform collectors enabled.

### Errors
| Condition | Status | Body |
|---|---|---|
| validation (missing/empty/whitespace/too long/unknown field) | 422 | FastAPI default detail shape |
| model not loaded | 503 | `{"detail": "model_not_loaded"}` |
| unexpected inference exception | 500 | `{"detail": "internal_error"}` (never leak traces) |
| Redis down during publish | 200 | normal body (fire-and-forget) |

## 3. Redis Streams event schema
- **Stream:** `mlobs:predictions`. Producer: `XADD mlobs:predictions MAXLEN ~ 50000 *` (O(1) approx trim; ~1+ day of demo traffic; Postgres is the durable store). Redis `appendonly no` (frozen).
- **Consumer group:** `pg_writer`, created idempotently at startup: `XGROUP CREATE mlobs:predictions pg_writer 0 MKSTREAM` (start id `0` so pre-startup events aren't lost; swallow BUSYGROUP). Consumer name `pg_writer-<hostname>`; exactly one instance in v1.
- **Fields (flat string map; parse contract):** `request_id` (UUID4), `ts_ms` (int epoch ms UTC), `text` (raw as received, ≤1000), `token_count` (int [3,256]), `label`, `confidence` (6dp), `model_version`, `latency_ms` (2dp).
- **Consumer loop (at-least-once):** ① `XREADGROUP GROUP pg_writer <consumer> COUNT 100 BLOCK 5000 STREAMS mlobs:predictions >` ② batch insert in ONE transaction with `INSERT ... ON CONFLICT (request_id) DO NOTHING` ③ only after COMMIT, pipelined `XACK`. Crash after commit pre-ack → redelivery → conflict no-op ("ack-after-commit + unique request_id = exactly-once effect" — the interview line). Malformed entry → poison path, never abort batch.
- **Recovery:** every 60s + at startup: `XAUTOCLAIM ... 60000 0-0 COUNT 100` through same insert path. **Poison pill:** delivery count > 5 (via `XPENDING ... IDLE 60000` detail) → log ERROR + XACK (drop) + `mlobs_consumer_events_dropped_total`.

## 4. PostgreSQL DDL (`sql/init.sql`, via /docker-entrypoint-initdb.d/)
```sql
CREATE TABLE predictions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id    UUID             NOT NULL UNIQUE,
    ts            TIMESTAMPTZ      NOT NULL,
    inserted_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    text          TEXT             NOT NULL CHECK (char_length(text) BETWEEN 1 AND 1000),
    token_count   SMALLINT         NOT NULL CHECK (token_count BETWEEN 3 AND 256),
    label         TEXT             NOT NULL CHECK (label IN ('positive', 'negative')),
    confidence    DOUBLE PRECISION NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    model_version TEXT             NOT NULL,
    latency_ms    DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0.0)
);
CREATE INDEX idx_predictions_ts ON predictions (ts DESC);

CREATE TABLE drift_runs (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    window_start_ts    TIMESTAMPTZ      NOT NULL,
    window_end_ts      TIMESTAMPTZ      NOT NULL,
    sample_count       INTEGER          NOT NULL CHECK (sample_count > 0),
    class_chi2_stat    DOUBLE PRECISION NOT NULL,
    class_drift        BOOLEAN          NOT NULL,
    length_chi2_stat   DOUBLE PRECISION NOT NULL,
    length_drift       BOOLEAN          NOT NULL,
    confidence_kl_nats DOUBLE PRECISION NOT NULL,
    confidence_drift   BOOLEAN          NOT NULL,
    drift_detected     BOOLEAN          NOT NULL,
    alert_sent         BOOLEAN          NOT NULL DEFAULT FALSE,
    bins               JSONB            NULL
);
CREATE INDEX idx_drift_runs_run_at ON drift_runs (run_at DESC);
```
Decisions: IDENTITY over SERIAL (SQL-standard); `request_id UNIQUE` = idempotency backstop AND the dedupe index; raw `text` stored (re-labeling/story value, no PII at demo scale); `idx_predictions_ts` is the only secondary index (sole query = latest-N window scan); `drift_runs` kept in SQL for replayable demo history beyond Prometheus retention; `bins JSONB` = per-test observed-vs-expected diagnostic arrays (only JSON in schema); skipped runs write NO row; no retention job (non-goal). No Alembic ⇒ `init.sql` runs only on fresh volume; schema change = `docker compose down -v` (documented tradeoff — never hand-patch live).

## 5. Drift detection spec
### Baseline
- One-shot `scripts/build_baseline.py` over the **full SST-2 validation split (872 sentences)** from checked-in `baseline/sst2_validation.tsv` (columns `sentence`, `label` — committed; no `datasets` dep, no network). Principled fixed reference; doesn't bake simulator quirks into baseline; reproducible from pinned model revision.
- Tokenization/inference byte-identical to API path (max_length=256, specials counted, float32).
- Output: committed `baseline/baseline.json`, mounted read-only into drift container:
```json
{"schema_version": 1, "model_version": "distilbert-sst2-v1", "created_at": "<ISO-8601 UTC>",
 "sample_count": 872,
 "class_probs": {"negative": 0.0, "positive": 0.0},
 "token_len_bin_edges": [3, 8, 16, 24, 32, 257],   "token_len_probs": [5 values],
 "confidence_bin_edges": [0.50,0.55,0.60,0.65,0.70,0.75,0.80,0.85,0.90,0.95,1.00],
 "confidence_probs": [10 values]}
```
- Bins are `[edge_i, edge_{i+1})`; last confidence bin closed `[0.95, 1.00]`. Token-length bins `[3,8) [8,16) [16,24) [24,32) [32,257)` — fat top bin lights up on injected long text. Confidence bins deliberately match the `mlobs_prediction_confidence_ratio` histogram edges.
- **Smoothing (build time):** add-one Laplace on `token_len_probs` + `confidence_probs` (`p_i=(count_i+1)/(N+K)`) ⇒ every baseline bin > 0 ⇒ chi² expected counts and KL denominators never zero. `class_probs` raw (~51/49).

### Window & cadence
- **Count-based sliding window: latest 500 predictions** (`SELECT label, token_count, confidence, ts FROM predictions WHERE model_version=$1 ORDER BY ts DESC LIMIT 500`). Count-based because chi² validity depends on expected counts n·p_i; time windows go invalid when traffic pauses.
- Long-lived loop, evaluates every **60s** (overlapping windows intended — monitor, not experiment).
- **Guard: < 200 rows → skip** (no drift_runs row; `mlobs_drift_runs_total{outcome="skipped_insufficient_samples"}`). At n=200 every bin with baseline p ≥ 2.5% has expected ≥ 5.

### Tests (all three per evaluation; any positive ⇒ drift_detected)
Statistic-vs-critical-value (α=0.01, hard-coded — identical to p<0.01, zero scipy; α=0.01 because ~1,440 tests/day/type at 60s cadence makes α=0.05 fire dozens of false alarms daily):
- **(a) Class chi²:** observed [n_neg, n_pos] vs expected n·class_probs. df=1. **Fire: Χ² > 6.635.**
- **(b) Token-length chi²:** 5 frozen bins vs n·token_len_probs. df=4. **Fire: Χ² > 13.277.** (Known approximation: smoothed top bin may have expected < 5 at n=200 — only makes the no-drift regime slightly conservative; it's exactly the bin drift injection floods.)
- **(c) Confidence KL:** window histogram (10 frozen bins, raw p_i, no smoothing) vs smoothed baseline q_i. **Direction: KL(P_window ‖ Q_baseline) = Σ p_i·ln(p_i/q_i)**, nats, convention 0·ln(0/q)=0. **Fire: KL > 0.10 nats** (PSI-informed: 0.10–0.25 = moderate shift boundary). Catches down-bin confidence mass from ambiguous/out-of-domain text even when class balance holds.

### Alerting
- Slack incoming webhook; payload `{"text": "[mlobs] DRIFT: <test> stat=<v> threshold=<v> window_n=<n> window=[<start>..<end>]"}`, one message per newly-firing test; httpx 5s timeout; delivery failure → log ERROR, skip, `alert_sent=false`, never crash loop.
- **Cooldown: 900s per test type**, in-memory monotonic timestamps (restart may re-alert once — acceptable, documented). `alert_sent=true` iff ≥1 message actually posted for the run.
- No "recovered" notification in v1 (non-goal); recovery visible as gauges falling in Grafana.

### Drift job metrics (`drift:9109` via start_http_server)
`mlobs_drift_class_chi2_stat` (G), `mlobs_drift_length_chi2_stat` (G), `mlobs_drift_confidence_kl_nats` (G), `mlobs_drift_detected` (G, label test∈{class,token_length,confidence}), `mlobs_drift_window_sample_count` (G), `mlobs_drift_runs_total` (C, outcome∈{evaluated,skipped_insufficient_samples,error}), `mlobs_drift_alerts_sent_total` (C, test), `mlobs_drift_last_run_timestamp_seconds` (G). Separate gauges per statistic — units differ.

## 6. Prometheus metric inventory
Convention: prefix `mlobs_`, base units + unit suffixes (`_seconds`, `_ratio`, `_total`); histograms in seconds (the API's `latency_ms` field is client convenience only).

**API (`api:8000/metrics`):**
| Metric | Type | Labels / buckets |
|---|---|---|
| `mlobs_http_requests_total` | C | endpoint(/predict,/health), method, status; /metrics excluded |
| `mlobs_http_request_duration_seconds` | H | endpoint; buckets .01,.025,.05,.075,.1,.15,.2,.3,.5,1.0,2.5 |
| `mlobs_inference_duration_seconds` | H | buckets .01,.02,.03,.04,.05,.075,.1,.15,.2,.3,.5,1.0 (delta vs request duration = framework overhead panel) |
| `mlobs_http_requests_in_flight` | G | /predict only |
| `mlobs_predictions_total` | C | label∈{positive,negative} |
| `mlobs_prediction_confidence_ratio` | H | buckets .5,.55,.60,.65,.70,.75,.80,.85,.90,.95 (+Inf) — matches drift bins |
| `mlobs_model_loaded` | G | model_version; 0/1 |
| `mlobs_stream_events_published_total` | C | successful XADDs |
| `mlobs_stream_publish_failures_total` | C | fire-and-forget loss counter |

**Consumer (`consumer:9108/metrics`):** `mlobs_consumer_events_consumed_total` (C), `mlobs_consumer_rows_inserted_total` (C), `mlobs_consumer_duplicates_skipped_total` (C — redelivery evidence), `mlobs_consumer_events_dropped_total` (C — poison pills), `mlobs_consumer_stream_lag_entries` (G — XINFO GROUPS lag), `mlobs_consumer_pending_entries` (G — XPENDING summary), `mlobs_consumer_batch_duration_seconds` (H — buckets .005,.01,.025,.05,.1,.25,.5,1.0).

**Drift (`drift:9109/metrics`):** the eight §5 metrics. Scrape config: `scrape_interval: 15s`, jobs api/consumer/drift by Compose DNS; default `python_*`/`process_*` collectors enabled everywhere.

## 7. Module ownership
`src/inference_service/` (FastAPI app, lifespan loader, schemas, **Redis producer**, API metrics — owns §2, producer half of §3, API rows of §6) · `src/consumer/` (XREADGROUP loop, XAUTOCLAIM, psycopg batch writer — consumer half of §3, writes §4, consumer metrics) · `src/drift/` (baseline loader, window query, chi²/KL, Slack, loop — owns §5) · `src/simulator/` (host-side generator: POSTs at RATE_RPS default 5 from normal corpus; `--mode drift` switches to long-text/negative-skewed/neutral corpus to trip all three tests) · non-Python roots: `sql/init.sql`, `baseline/` + `scripts/build_baseline.py`, `docker/` + `docker-compose.yml`, `prometheus/prometheus.yml`, `tests/{inference_service,consumer,drift}/`. **No shared `src/common/`** — each service owns its own pydantic-settings `config.py`; cross-service constants are frozen by this document, not by imports ⇒ disjoint directories, zero cross-slice merge conflicts.

## Appendix A — cross-service frozen constants
| Constant | Value |
|---|---|
| Stream / group / MAXLEN | `mlobs:predictions` / `pg_writer` / `~ 50000` |
| Model version string | `distilbert-sst2-v1` |
| Tokenizer | max_length=256, token_count includes specials |
| Window / guard / cadence | 500 rows / 200 min / 60s |
| Thresholds | Χ²>6.635 (df1) · Χ²>13.277 (df4) · KL>0.10 nats |
| Alert cooldown | 900s per test |
| Ports | api 8000 · consumer-metrics 9108 · drift-metrics 9109 · redis 6379 · postgres 5432 · prometheus 9090 · grafana 3000 |

## Appendix B — verification matrix (all NOT RUN — design phase)
| Scope | Check | Evidence | Owner |
|---|---|---|---|
| deps | pins resolve on py3.12-slim, CPU torch, image < 2GB | docker build + pip check | S1 |
| api | §2 contract incl. 422/503, whitespace rejection | pytest tests/inference_service | S1 |
| consumer | replay same event twice → 1 row | pytest w/ ephemeral PG | S2 |
| consumer | poison pill dropped after 6 deliveries + XACKed | unit test recovery path | S2 |
| drift | chi²/KL vs hand-computed fixtures (0·log0, smoothing) | pytest tests/drift | S3 |
| drift | baseline probs sum to 1; bins match frozen edges | assertion in builder + unit test | S3 |
| e2e | drift mode fires all 3 tests < 5 min; Slack posts; drift_runs rows | compose up + simulator --mode drift | S5 |
| obs | all §6 names present in Prometheus | curl :9090 label values \| grep mlobs_ | S5 |
| lint | ruff clean | ruff check src tests | all |

# v1.1 Design Specification (FROZEN)

> v1.1 stretch item 1 — **shadow/canary second-model comparison on live traffic**.
> Transcribed here so docs/PLAN.md stays the single source of truth. The v1 spec
> above is unchanged and remains frozen; this section governs v1.1 work only.

## v1.1 scope

Score a candidate model against identical live traffic, compare agreement /
confidence deltas / latency / per-model drift on dashboards, and support a
written "promote or hold?" decision — with **zero changes to the primary
prediction path**. The prediction event on `mlobs:predictions` already carries
the raw text plus the primary model's label/confidence/latency, so a shadow
scorer joining the stream with its own consumer group gets scoring input *and*
the primary half of every comparison in one message.

**OUT (v1.1):** canary routing (deferred, gated on shadow evidence), automated
promotion, A/B significance testing, MLflow, k8s, >2 models, retraining,
retention jobs, Alembic.

## Architecture decisions (ADR summary — frozen)

- **D1 Topology:** new container `shadow-scorer` joins `mlobs:predictions` with
  consumer group `shadow_scorer` (start id `$` — score from deploy forward, no
  backfill), re-scores `text` with the candidate model. Shadow crash/lag never
  touches the primary path; lag is observable and drains at ~7–8× catch-up.
- **D2 Model risks (retired in Slice A):** candidate has no meaningful
  `id2label` → freeze map `LABEL_0→negative`, `LABEL_1→positive`; the build-time
  bake asserts two sanity probes (one clearly positive, one clearly negative
  sentence) so a wrong map fails the Docker build. Only `.bin` weights on the Hub
  + transformers 5.13.1 → if the pickle load is refused, the bake converts once
  at build time to safetensors (`torch.load(weights_only=True)` → `load_state_dict`
  → `save_pretrained`); the runtime never touches pickle.
- **Tokenizer rule (frozen):** the shadow computes `token_count` with its OWN
  tokenizer (never the event's), max_length=256, specials counted, clamp [3,256].
- **D3 Storage:** new table `shadow_predictions` (`request_id UUID NOT NULL
  UNIQUE` exactly-once backstop; ts = original event ts; model_version;
  label/confidence/token_count/latency_ms with the same CHECKs as `predictions`;
  **denormalized `primary_label` + `primary_confidence`** from the event → join-free
  agreement SQL, no race vs the pg_writer commit, deliberately no FK).
- **Migration stance:** no Alembic; `sql/init.sql` stays canonical for fresh
  volumes **plus** one idempotent delta `sql/migrations/002_shadow.sql`
  (`CREATE TABLE IF NOT EXISTS` + `drift_runs.model_version TEXT NOT NULL DEFAULT
  'distilbert-sst2-v1'`). Migration "001" is implicitly the v1 `sql/init.sql`.
- **D4 Metrics/dashboards:** shadow scorer exports on **:9110**, prefix
  `mlobs_shadow_`: `comparisons_total{primary_label,shadow_label}` (4-cell
  confusion matrix; agreement derived in PromQL), `confidence_delta` H
  (d = p_pos(shadow) − p_pos(primary); buckets −0.4,−0.2,−0.1,−0.05,−0.02,0.02,
  0.05,0.1,0.2,0.4), `inference_duration_seconds` H (same buckets as api),
  `predictions_total{label}`, `confidence_ratio` H (same 0.50–1.00 bins), plus
  consumer-style health: `events_scored_total`, `rows_inserted_total`,
  `duplicates_skipped_total`, `events_dropped_total`, `stream_lag_entries`,
  `pending_entries`. New third dashboard "mlobs — Model Comparison" (Slice C);
  the two existing dashboards get one mechanical edit: pin drift panels to
  `job="drift"`.
- **D5 Multi-model drift:** refactor `src/drift/` identity-only — `MODEL_VERSION`
  moves from `constants.py` to `DriftSettings` (env, default `distilbert-sst2-v1`),
  new `SOURCE_TABLE` setting whitelisted to `{predictions, shadow_predictions}`,
  `validate_baseline` checks the configured version, Slack text prefixed
  `[mlobs][<model_version>]`, `drift_runs` insert includes model_version. Compose
  adds `drift-shadow` (same image; `MODEL_VERSION=minilm-sst2-v1`,
  `SOURCE_TABLE=shadow_predictions`, `BASELINE_PATH=/baseline/baseline-minilm.json`).
  Prometheus jobs `drift` vs `drift_shadow`; metric names unchanged. Window /
  guard / cadence / thresholds / bins stay frozen.
- **D6 Image/module ownership:** new `src/shadow_scorer/` (own
  config/model/scorer/parsing/metrics — consciously duplicates the ~90-line model
  wrapper; **no shared src/ rule preserved**), `docker/shadow_scorer.Dockerfile`
  (api pattern: CPU torch first, build-time bake + label-map assert, offline env,
  non-root), `requirements/shadow_scorer.txt` (same coupled torch/transformers/
  tokenizers pins as api).
- **D8 Resources:** steady-state ≈2.4–2.8GB of 4GB (≥1GB headroom). Shadow torch
  threads = 1 (frozen). Compose `mem_limit`: `api` 1536m, `shadow-scorer` 768m
  (only the two torch containers).

## v1.1 frozen constants (new components)

| Constant | Value |
|---|---|
| Shadow group / start / consumer | `shadow_scorer` / `$` (MKSTREAM, swallow BUSYGROUP) / `shadow_scorer-<hostname>` |
| Read batch / poison | `COUNT 20 BLOCK 5000` / drop after 5 deliveries |
| Shadow model / revision / version | `philschmid/MiniLM-L6-H384-uncased-sst2` @ `0c0ecdc39368f87291727ec084111e89e30b45b2` / `minilm-sst2-v1` |
| Label map | `LABEL_0→negative`, `LABEL_1→positive` (bake-asserted) |
| Shadow tokenizer | own tokenizer, max_length=256, specials counted, token_count ∈ [3,256] |
| Shadow torch threads | 1 |
| Table | `shadow_predictions` + `idx_shadow_predictions_ts (ts DESC)`; no FK |
| Migrations | `sql/init.sql` + idempotent `sql/migrations/002_shadow.sql` |
| Shadow metrics | port 9110, prefix `mlobs_shadow_` |
| Confidence delta | d = p_pos(shadow) − p_pos(primary), buckets above |
| Prometheus jobs | `shadow_scorer → shadow-scorer:9110`, `drift_shadow → drift-shadow:9109` |
| Drift env (new) | `MODEL_VERSION` (default `distilbert-sst2-v1`), `SOURCE_TABLE` (default `predictions`) |
| Shadow baseline | `baseline/baseline-minilm.json` (schema_version 1, 872-row TSV, shadow tokenizer) |
| Mem limits | api 1536m, shadow-scorer 768m |

## v1.1 slices (sequenced A → B → C; disjoint dirs)

- **Slice A — shadow scorer service.** `src/shadow_scorer/**`,
  `docker/shadow_scorer.Dockerfile`, `requirements/shadow_scorer.txt`, all of
  `sql/` (init.sql + `migrations/002_shadow.sql` incl. the drift_runs column — one
  owner for sql/), compose `shadow-scorer` service + both mem_limits,
  `tests/shadow_scorer/**`, CI shadow-image build/size job, this PLAN.md section.
  *Acceptance:* docker build succeeds with the label-map sanity assertion passing
  (retires the `.bin`/label risks); unit tests — parsing, idempotent insert (same
  event twice → 1 row), poison drop, confidence-delta math; ruff clean.
- **Slice B — multi-model drift.** `src/drift/**` (identity → settings,
  SOURCE_TABLE whitelist, baseline validation, Slack prefix, drift_runs
  model_version), `scripts/build_baseline.py` flags, `baseline/baseline-minilm.json`,
  compose `drift-shadow` service, `tests/drift/**` updates. *Acceptance:*
  default-env drift behavior byte-identical; primary baseline rebuild bit-identical;
  shadow-configured drift evaluates `shadow_predictions`; Slack text carries
  model_version.
- **Slice C — comparison observability + rollout + release.**
  `prometheus/prometheus.yml` (2 new jobs), `grafana/dashboards/model_comparison.json`
  + `job="drift"` pin in the 2 existing dashboards, README (shadow story,
  promotion criteria with a no-ground-truth caveat, refreshed load/RAM numbers),
  EC2 rollout (one-time psql migration + redeploy + load-test re-run). *Acceptance:*
  live e2e matrix (comparison dashboard, both-model drift, /predict p95 delta ≤10%
  shadow on vs off, kill-test drains to zero duplicate rows, disagreement
  inspectable by request_id). Close-out: tag `v1.1.0`.

# Phase 2 P1 — ADR summary (D9–D16)

> Decisions taken while codifying the already-running EC2 shadow-test host as
> Terraform (Phase 2 slice P1, `infra/`). The v1 and v1.1 specifications above
> are frozen and untouched; this block appends only. **D7 is absent on
> purpose** — the number was never issued, and the gap is not backfilled.

- **D9 Remote state and version bands:** state in S3
  (`mlobs-tfstate-601548053958`, key `ec2/terraform.tfstate`, us-west-2) with
  `use_lockfile = true`; Terraform `~> 1.14.0`, AWS provider `~> 6.33`.
  *Rejected:* local state, and a DynamoDB lock table. *Why:* local state cannot
  be read by CI and dies with the laptop; DynamoDB locking is deprecated
  upstream and would add a second billed resource for a single-writer root, so
  the lock is an S3 object instead. The provider is held to a MINOR band so
  Dependabot can offer 6.x bumps as reviewable PRs while a 7.x major — a
  breaking-schema event for imported resources — needs a deliberate edit.
  Backend blocks cannot interpolate, so the account id is a literal; account
  and resource ids are accepted exposure for this repo, the SSH CIDR is not.
- **D10 Import in place, never recreate:** `import` blocks bind the instance,
  the security group and each of its four rules to their real ids; every
  attribute transcribes what `describe-*` returned on 2026-09-01. The AMI is a
  literal `var.ami_id`, the default VPC and subnet are read-only data sources,
  and outputs are limited to `instance_id` and `sg_id`. *Rejected:* a clean
  destroy-and-recreate from code; `data "aws_ami"` most-recent lookup; managing
  the default VPC; outputting `public_ip`/`public_dns`. *Why:* this instance
  carries the certified `hey` numbers in README.md — recreating it invalidates
  every EC2 figure the repo publishes. An AMI lookup re-resolves on each plan
  and `ami` forces replacement, so the next Canonical publish would silently
  turn a read-only CI plan into a queued destroy of that host. The address
  attributes change on every stop/start and would be stale as soon as written
  down. Consequence, stated rather than hidden: this slice proves the host is
  *described* by code, not that it can be *rebuilt* from code — that evidence
  is deferred to P3's ephemeral apply/destroy run.
- **D11 Bootstrap is a runbook, not automation:** the state bucket, the OIDC
  provider, the plan role and the initial import apply are performed once by the
  owner from `infra/README.md`. *Rejected:* a second Terraform root that
  provisions the first root's backend. *Why:* Terraform cannot create its own
  state bucket, and a bootstrap root only moves the chicken-and-egg one level
  down while adding a second state to own — four documented CLI calls are the
  smaller answer. The runbook ends in evidence, not assertions: a
  `terraform state pull` secret sweep, and `plan -detailed-exitcode` returning
  0 with the instance **both stopped and running**, since attributes such as
  `public_ip` and `instance_state` only populate in one of those.
- **D12 One flat root, no module layer:** `infra/ec2/*.tf` split by concern
  (versions, backend, providers, network, compute, iam, variables, outputs,
  imports). *Rejected:* a `modules/` + `envs/` tree. *Why:* there is exactly one
  instantiation of this configuration and no second consumer to parameterise
  for, so a module boundary would add indirection and buy no reuse. It becomes
  worth revisiting when a second environment exists.
- **D13 CI: validate everywhere, plan under OIDC:** `TerraformValidate` is
  credential-less (`fmt -check -recursive`, `init -backend=false`, `validate`)
  and therefore also covers fork and Dependabot branches; `TerraformPlan` runs
  only for same-repo events, assumes `mlobs-tf-plan` through GitHub OIDC, and
  plans with `-lock=false`. The role's trust policy pins `aud` and enumerates
  `sub` to exactly the `pull_request` and `ref:refs/heads/main` subjects; its
  permissions are a hand-written policy covering `ec2:Describe*`, read of the
  state object, and read of the two IAM resources this root manages. *Rejected:*
  a long-lived IAM user access key in Actions secrets; the AWS-managed
  `ReadOnlyAccess`; a `repo:owner/name:*` wildcard subject; write access to the
  `.tflock` object. *Why:* no static AWS credential exists to leak or rotate; a
  wildcard subject would let any workflow in the repository assume the role;
  `ReadOnlyAccess` grants read across every service in the account rather than
  the read surface of this root; and with no lock-write grant the job
  structurally cannot corrupt state or strand a lock. Actions are SHA-pinned in
  the plan job only — it is the one job holding an AWS session.
- **D14 Defaults transcribe reality; two default tags:** `var.region`
  `us-west-2`, `var.instance_type` `t3.medium`, `var.ami_id`, `var.key_pair_name`
  and `var.availability_zone` all default to the as-found values, and the
  provider stamps `project=mlobs`, `managed-by=terraform`. *Rejected:* defaults
  chosen for tidiness rather than fidelity; renaming resources to a convention.
  *Why:* when every default matches the account, "a plan reports nothing to do"
  becomes the acceptance assertion. Nothing as-found conflicts with the two
  default tags, so reality is kept and the tags ride on top; the first apply
  adds them in place — verified locally as `6 to import, 3 to add, 6 to change,
  0 to destroy`.
- **D15 No secret material in version control:** `var.ssh_ingress_cidr` is
  `sensitive`, has no default, and arrives from a gitignored `terraform.tfvars`
  locally and the `SSH_INGRESS_CIDR` Actions secret in CI. *Rejected:* a
  committed default, even of a placeholder that invites editing in place.
  *Why:* the value is the operator's home address. Recorded honestly, because it
  is easy to over-trust: Terraform's `sensitive` marking hides values *derived
  from* the variable but **not** `aws_security_group`'s API-read `ingress`
  attribute, which prints in clear text whenever the group appears in a diff.
  What actually protects the public Actions log is GitHub's secret masking, so
  the secret must be stored in the exact wire form `A.B.C.D/32`. The value also
  lands in remote state in clear text — hence a private, versioned, encrypted
  bucket and a state sweep as the closing bootstrap step.
- **D16 Layout and repo plumbing:** `infra/README.md` plus the `infra/ec2/`
  files above; `.gitignore` gains `.terraform/`, `*.tfstate*`, `*.tfplan` and
  `terraform.tfvars` while `.terraform.lock.hcl` is deliberately committed;
  the DocsGate overclaim sweep extends to `infra/`; Dependabot gains the
  `terraform` ecosystem at `/infra/ec2`, weekly. *Rejected:* ignoring the lock
  file; leaving `infra/` outside the vocabulary gate. *Why:* the lock file is
  what makes CI and a laptop resolve identical provider builds (hashes are
  recorded for `linux_amd64` and `darwin_arm64`), and an IaC directory with a
  cost note is exactly where unchecked claims accumulate.

# Phase 2 P2 — ADR summary (D17–D27)

> Decisions taken while moving the stack from Docker Compose to k3s on the same
> EC2 host (Phase 2 slice P2, `deploy/k3s/`). The v1 and v1.1 specifications
> above are frozen and untouched; this block appends only. Slice P2a lands the
> manifests, the deploy scripts and the CI rehearsal; P2b the on-host cutover
> and its runbook; P2c the deploy pipeline. Decisions covering later slices are
> recorded here because they were taken together and constrain what P2a builds.

- **D17 Plain manifests, no templating layer:** `deploy/k3s/manifests/*.yaml`
  applied with `kubectl apply -f`, with two shell scripts around them —
  `apply.sh` to render and deploy, `smoke.sh` to check. The only substitutions
  are an image prefix, an image tag and a config hash, done with `sed` into a
  temporary directory. *Rejected:* Helm; Kustomize. *Why:* there is one
  instantiation of this stack and no second environment to parameterise for, so
  a chart would buy indirection and no reuse — the same argument as D12 for
  Terraform modules. It also keeps the manifests readable as evidence: a
  stranger comparing them against `docker-compose.yml` reads YAML, not a
  template language. Helm evidence is a stated P3 goal and belongs there, where
  an ephemeral EKS cluster gives it something to prove.
- **D18 GHCR for the three licensed images; the shadow image is not
  published:** `api`, `consumer` and `drift` are built and pushed by CI on
  every push to `main` as `ghcr.io/muratalkan06/mlobs-<svc>` under two tags —
  the immutable commit SHA, which a deploy pins, and a moving `main`. The
  shadow scorer is **excluded**: the candidate model it bakes is published on
  Hugging Face with no upstream licence stated, so republishing those weights
  inside a container image on a public registry is a redistribution this
  repository will not make (owner ruling 2026-09-10). Its image is delivered as
  a private S3 tarball in P2c, with a local build plus `scp` as the P2b interim
  runbook. *Rejected:* publishing all four and hoping; vendoring the weights
  behind a licence assertion the repository is not in a position to make;
  Docker Hub. *Why:* the posture that survives scrutiny is that the weights stay
  where their author put them — publicly hosted, fetched verbatim at build
  time — and image redistribution is avoided entirely rather than argued about.
  GHCR needs no account beyond the one this repository already has and takes a
  per-run `GITHUB_TOKEN` rather than a long-lived credential.
- **D19 k3s pinned, batteries removed, host ports kept:** k3s
  `v1.36.4+k3s1` (container tag `rancher/k3s:v1.36.4-k3s1`) with `traefik`,
  `servicelb` and `metrics-server` disabled. The api and Grafana pods keep
  `hostPort` 8000 and 3000; the EC2 security group is not touched. *Rejected:*
  an Ingress; a LoadBalancer Service; `NodePort` in the 30000–32767 range;
  letting k3s pick its own version. *Why:* the security group already admits
  exactly the two ports the README tells a reader to open, and every
  alternative changes them — a NodePort moves the port a stranger has to type,
  and a LoadBalancer on a single node is servicelb re-implementing a hostPort
  through an extra hop. The three disabled add-ons cost RAM on a 4GB box for
  capabilities the manifests never reference. The version is pinned for the
  same reason the AMI is (D10): an unpinned control plane re-resolves and the
  host stops being the thing that was tested.
- **D20 One PVC, everything else ephemeral:** Postgres gets a `local-path` PVC
  (5Gi) mounted at `/var/lib/postgresql/data` with `subPath: pgdata`.
  Prometheus, Grafana and Redis run entirely on the container filesystem.
  *Rejected:* PVCs for the Prometheus TSDB and the Grafana sqlite database;
  Longhorn or any replicated volume. *Why:* Redis was already `--appendonly no`
  under Compose and the consumer's recovery path assumes redelivery, not
  durability; Grafana's dashboards and datasource are provisioned read-only
  from the repository on every start, so the only thing its database holds is
  ad-hoc UI state; and the Prometheus retention worth keeping is nil on a demo
  box. One PVC is also one thing to delete for a schema change (PRINCIPLES.md
  §4: schema changes are never hand-patched onto a live volume). A replicated
  volume on a single node is a word, not a property. *Consequence:* README
  prose describing Compose volumes is now partly wrong for the k3s runtime; the
  erratum correcting it rides P2b with the rest of the cutover documentation,
  rather than being written before the runtime it describes exists.
- **D21 One scrape config, mounted verbatim:** `prometheus/prometheus.yml` is
  turned into a ConfigMap by `apply.sh` at deploy time and mounted unchanged —
  no copy under `deploy/`. The Services are therefore named exactly as the
  Compose DNS names the file already contains (`api`, `consumer`, `drift`,
  `shadow-scorer`, `drift-shadow`), and the whole stack lives in one namespace
  so those short names resolve. The Prometheus pod template carries an
  `mlobs/config-hash` annotation holding the sha256 of that file. *Rejected:* a
  k3s-specific copy of the scrape config; `<svc>.<ns>.svc.cluster.local`
  targets; Prometheus service discovery via the Kubernetes API. *Why:* two
  copies of a scrape config are two files that will disagree, and the disagreement
  shows up as a silently missing target. Service discovery would be the right
  answer for a cluster with churn and is the wrong answer for five static
  targets whose names are frozen in Appendix A. The annotation exists because
  editing a ConfigMap restarts nothing — and this one is mounted with `subPath`,
  which does not even get the kubelet's in-place refresh — so without it a
  config change would apply nowhere; stamping the hash makes the pod template
  differ exactly when the config differs, so a repeated deploy is a real no-op.
- **D22 Workload kinds follow identity, probes follow meaning:** `consumer` and
  `shadow-scorer` are StatefulSets, because each registers in a Redis consumer
  group under its own hostname and a stable pod name is therefore a stable
  identity across rollouts. `api`, `grafana` and `postgres` use
  `strategy: Recreate`. The api's three probes read one `/health` three ways: a
  startup probe covering the model load, a readiness probe that treats 200
  `degraded` as ready, and a `tcpSocket` liveness probe. Compose
  `depends_on: service_healthy` becomes blocking initContainers. The v1.1 D8
  memory limits are transcribed verbatim (`api` 1536Mi, `shadow-scorer` 768Mi);
  every workload also carries a memory request. *Rejected:* Deployments for the
  stream consumers; RollingUpdate on the hostPort workloads; `/health` as the
  liveness probe; limits on the non-torch services. *Why:* a Deployment mints a
  new random pod suffix on every rollout, stranding the previous name's pending
  entries until XAUTOCLAIM sweeps them — and the shadow group starts at `$`,
  which makes a lost identity unrecoverable rather than merely slow. A
  RollingUpdate behind a hostPort deadlocks: the incoming pod cannot bind a port
  the outgoing pod still holds, and the outgoing pod is not removed until the
  incoming one is ready. `/health` as liveness would restart the api because
  *Redis* is unwell (`degraded` is a 200 by design, and a restart cannot fix
  another service) or loop it forever on a model that will not load. Requests
  are what let the scheduler refuse to overcommit a 4GB node; limits beyond D8's
  two are guesses that would turn a memory spike into an OOM kill.
- **D23 One Secret from the host `.env`, under an output-hygiene contract:**
  `apply.sh` builds `mlobs-secrets` from the same gitignored `.env` Compose
  used, via `kubectl create secret --from-env-file --dry-run=client -o yaml |
  kubectl apply -f -`. Three rules hold in that script and are written down in
  it: xtrace is explicitly disabled rather than merely unused; no secret value
  is ever read into a shell variable, passed as an argument or interpolated into
  a string (required keys are checked by `grep -q`, which reports only whether a
  pattern matched); and every command that could echo secret material has its
  output discarded, with failures reported as fixed lines containing no input.
  Postgres DSNs are assembled in the pod from a `secretKeyRef` `POSTGRES_PASSWORD`
  through Kubernetes' `$(VAR)` dependent expansion. Compose's
  `${GF_ADMIN_USER:-mlobsadmin}` default is materialised into the Secret by
  `apply.sh`, since a manifest has no shell-style default. *Rejected:* committed
  SealedSecrets or SOPS; a full DSN stored as a Secret key; External Secrets
  Manager. *Why:* the credential already lives in one gitignored file on one
  host, and a sealing tool would add a key to manage and a second place for the
  truth to live — it becomes the right answer when there is a second
  environment. Building the DSN in the pod rather than storing it whole removes
  the `localhost` fallback DSN from the runtime picture: a service with a broken
  reference fails to start instead of quietly connecting to nothing, which is
  the failure class that cost a debugging session under Compose. The hygiene
  rules are written as rules because each is easy to undo by accident, and this
  repository's logs are public.
- **D24 Deploy is SSM under OIDC, through a document that cannot run arbitrary
  commands:** the deploy job assumes a role by GitHub OIDC and calls
  `ssm:SendCommand` against a custom SSM document whose only parameter is a
  commit SHA, pattern-validated to 40 hex characters. The role's trust is gated
  on a GitHub `environment` (`ec2-deploy`) with a required reviewer; the job
  first checks the SHA is an ancestor of `main` and that the three images exist
  in GHCR. The role is not granted `ec2:StartInstances`. *Rejected:* SSH from
  Actions with a stored private key; `AWS-RunShellScript` with a
  workflow-supplied command string; a self-hosted runner on the box; letting the
  pipeline start a stopped instance. *Why:* an SSH key in Actions secrets is a
  long-lived credential to leak and rotate, which is the thing P1 removed.
  `AWS-RunShellScript` grants remote code execution to anyone who can trigger
  the workflow, so the document is a fixed script with one validated argument
  instead. The ancestor check stops a deploy of an arbitrary branch, the image
  preflights turn a missing tag into a failed check rather than a half-deployed
  host, and withholding `StartInstances` keeps the cost story honest: the box is
  started deliberately by its owner, never by a merge.
- **D25 Rollback is a redeploy of the previous SHA, with the database handled
  first:** roll back by re-running the deploy against the previous commit SHA;
  `kubectl rollout undo` is the faster path when the previous ReplicaSet is
  still present. `smoke.sh` is the single check for both CI and the host, so a
  green rollback means what a green deploy means. **A `pg_dump` is taken before
  cutover and restored after a rollback** (owner ruling 2026-09-10). Compose is
  retained on the box as a fallback runtime for the duration of P2, and the
  abort runbook takes an EBS snapshot before any destructive step. *Rejected:*
  rollback by rebuilding an image from an older tree; deleting the Compose
  stack at cutover; treating `rollout undo` as sufficient on its own. *Why:*
  per-SHA tags exist precisely so that rolling back is deploying something that
  already built and already passed; rebuilding reintroduces the risk that a
  dependency resolved differently. `rollout undo` cannot help once the
  ReplicaSet has been pruned, so it is the shortcut and not the procedure. The
  database is the part a rollback cannot re-derive: the same volume is reused
  across runtimes, so a schema or data change made under k3s outlives the
  rollback unless it is dumped first. Keeping Compose installed costs disk and
  buys a runtime that is known to work while the new one is still being trusted.
- **D26 The cutover is gated on a measured rehearsal, not an estimate:** P2b
  runs the stack under k3s on the real `t3.medium` and records the result. Pass
  is `MemAvailable` at or above 400MB and zero OOM kills during a sustained
  5 rps load with the shadow scorer running. A fail moves the host to
  `t3.large` by changing `var.instance_type` in the P1 Terraform root.
  *Rejected:* accepting the k3s overhead as "small enough"; sizing up
  pre-emptively; dropping the shadow scorer to make the numbers fit. *Why:* k3s
  adds a control plane the Compose measurements never included, and D8's budget
  had roughly 1GB of headroom on 4GB — enough that the question is real and not
  enough that it can be waved through. Sizing up first would hide whether the
  migration cost anything, which is the one number this slice can honestly
  report. Fixing it through the Terraform root rather than the console keeps P1's
  claim true: the instance is described by code.
- **D27 Compose-era numbers are relabelled, not deleted; re-measurement is its
  own slice:** the existing `hey` figures in README.md are annotated as
  historical, naming the runtime and the `t3.medium` instance type they were
  taken on. The k3s re-measurement is appended in P2b under the same
  methodology. Retiring `docker-compose.yml` is deferred to P3. *Rejected:*
  deleting the old numbers; quietly reusing them for the k3s runtime; retiring
  Compose at cutover. *Why:* PRINCIPLES.md §6 makes a performance number
  inseparable from its methodology, and the runtime is part of the methodology —
  reusing a Compose figure for k3s would be exactly the divergence between
  stated claim and built reality this repository exists to avoid. Deleting the
  figures instead would throw away a real measurement and the before-and-after
  comparison that makes the migration legible. Compose stays until the k3s path
  has been the live one long enough to have earned it (D25's fallback), and
  removing it is a change with its own risk and therefore its own slice.

> **Erratum (2026-09-27, P2b):** The v1 volume prose — §4's "`init.sql` runs
> only on fresh volume; schema change = `docker compose down -v`", the
> `docker-compose.yml` comment to rotate the Postgres password "by recreating the
> volume", and the "live volume" wording that followed — reads as if Postgres
> data sat on a persistent volume that only `down -v` resets. No such volume
> existed. `docker-compose.yml` declares no named volumes, so the data directory
> lived in the anonymous volume Docker creates for the `postgres:16-alpine`
> image's `VOLUME /var/lib/postgresql/data`. Compose carries an anonymous volume
> across an `up` that recreates the container, but it has no stable name and a
> later `up` does not remount it, so a plain `docker compose down` would have
> started the next `up` from an empty initdb: the demo history was durable
> across restarts and recreations, but not across a `down`. D20's `local-path`
> PVC (Postgres only) is what makes the documented durability true. D25's "the
> same volume is reused across runtimes" did not hold either: the history
> crossed the cutover by `pg_dump --clean --if-exists` and restore, verified by
> row counts — predictions 8386 → 8386, shadow_predictions 3628 → 3628,
> drift_runs 18884 → 18885 (the +1 is a fresh k3s drift cycle). The frozen text
> is not edited.

## Phase 2 P2c — ADR addendum (D28–D30)

> Decisions taken while building the deploy pipeline D24 describes (Phase 2
> slice P2c: `infra/ec2/deploy.tf`, the `ShadowPublish` job in
> `.github/workflows/ci.yml`, and `.github/workflows/deploy.yml`). D17–D27 and
> the erratum above are untouched; this block appends only.

- **D28 The shadow image travels as a private S3 tarball, read by the host's
  own role:** on every push to `main`, after `K3sSmoke` passes, `ShadowPublish`
  builds the shadow-scorer image as
  `ghcr.io/muratalkan06/mlobs-shadow-scorer:<sha>` — the ref the manifest
  renders at `IMAGE_TAG=<sha>`, never pushed to any registry — saves it with
  `docker save | gzip`, and uploads it to
  `s3://mlobs-artifacts-601548053958/shadow/<sha>.tar.gz` as
  `mlobs-artifact-publish` (trust: the `shadow-publish` environment subject;
  grant: `s3:PutObject` under `shadow/`, nothing else). The bucket is managed by
  the P1 Terraform root: public access block on, SSE-S3, a TLS-deny bucket
  policy, versioning off, `shadow/` objects expired after 60 days and abandoned
  multipart uploads after 7. The deploy document downloads the tarball on the
  host and imports it with `k3s ctr`. Private S3 is the owner's F7d ruling,
  consistent with D18's licensing position. **Two additions to the design as
  frozen at the gate, disclosed here and commented where they are granted:**
  the host's instance role holds `s3:GetObject` under `shadow/` beside
  `AmazonSSMManagedInstanceCore`, because the host itself fetches the tarball;
  and `mlobs-deploy` holds `s3:ListBucket` on the bucket, conditioned on the
  request prefix being exactly `shadow/`, for the workflow's tarball preflight
  (`list-objects-v2`, the one key picked out client-side). **Security review of
  PR #57, adopted before merge:** that preflight first used `head-object`,
  which S3 authorises as `s3:GetObject` — a download right for a role that
  only needs to know a key exists — so the grant was narrowed to the
  prefix-scoped list. `mlobs-artifact-publish` first trusted the
  `refs/heads/main` subject, which GitHub issues to every job on `main`; since
  the host imports and runs whatever sits under `shadow/`, it now trusts the
  `shadow-publish` environment — no reviewer, deployments from `main` only —
  which `ShadowPublish` declares. Both environments are created, and read
  back, before the apply that creates the roles trusting them. The
  credentialed `TerraformPlan` job pins Terraform to an exact patch (1.14.9)
  rather than a range, so the binary that runs beside the AWS session changes
  only by commit. And all three S3 grants carry an `s3:ResourceAccount`
  condition on 601548053958: a grant by bucket name alone would follow the
  name to another account if the bucket were ever deleted and the name
  reclaimed. *Rejected:* the public-registry route D18 already excludes; keeping P2b's
  interim of a local build copied up with `scp`. *Why:* the weights stay out of
  every registry, and the tarball moves with no stored credential anywhere —
  the runner writes it with a per-run OIDC session, and the host reads it with
  instance-role credentials served over IMDSv2, whose hop limit of 1 keeps them
  out of the pods' reach. `scp` needs an SSH key held somewhere, which is the
  long-lived credential P1 removed. *Consequence:* 60 days is also the shadow
  image's rollback window. A redeploy of an older commit fails the tarball
  preflight, loudly, and the way back from there is a revert commit on `main`.
- **D29 The deploy gate is layered, and the workflow file is not one of the
  layers:** `deploy.yml` is `workflow_dispatch` only and runs in the GitHub
  environment `ec2-deploy`, which the owner configures with a required reviewer
  and deployments from `main` only (`infra/README.md`, "Deploy pipeline").
  `mlobs-deploy` trusts exactly one subject,
  `repo:MuratAlkan06/ml-observability-system:environment:ec2-deploy`, with
  `aud` pinned; GitHub issues it only to a job in that environment, and only
  after approval. The role may `ssm:SendCommand` the `mlobs-deploy` document
  to the one instance and nothing else. `ssm:GetCommandInvocation` is granted
  on `*` because the action defines no resource type, and
  `ec2:DescribeInstances` on `*` because EC2 Describe is not resource-scopable.
  There is no `ec2:StartInstances`, no `ssm:CancelCommand` and no document
  write. The document is a fixed script with one parameter, `Sha`, whose
  `allowedPattern` `^[0-9a-f]{40}$` is enforced by the SSM API and again by the
  agent; before checking anything out, the script re-runs
  `git merge-base --is-ancestor` against a freshly fetched `origin/main`. As
  hardened by the security review of PR #57, it also refuses a checkout with
  edited tracked files, downloads and imports the tarball before the checkout
  so that a failed download leaves the working tree where it was, and after
  the checkout refuses unless `HEAD` is exactly the SHA — `git checkout`
  prefers a local branch of that name over the commit the ancestry check
  resolved. Any change to the document's content deletes the superseded
  versions (`aws ssm delete-document --name mlobs-deploy --document-version
  <n>`, the version always named: without it the whole document is deleted)
  or renames the document. Older versions stay callable, since `SendCommand`
  takes a document version and no `ssm:DocumentVersion` condition key exists
  for it. The workflow repeats the pattern and ancestry checks client-side,
  preflights the three GHCR manifests, the S3 tarball and the instance's
  `running` state, and runs in its own concurrency group without
  `cancel-in-progress`. *Rejected:*
  trusting the `refs/heads/main` subject for the deploy role;
  `AWS-RunShellScript` (D24); letting the pipeline start a stopped host (D24);
  cancelling an in-flight deploy for a newer one. *Why:* the main subject is
  issued to every job that runs on `main`, so trusting it would put root on the
  host one merged workflow edit away with no human in the loop — the
  environment subject exists only behind the reviewer. The document rather than
  the workflow is the boundary because the workflow is text anyone with push
  access can change; whatever a changed workflow sends, the host runs the same
  script, and only against a commit that is on `main`. Cancelling the workflow
  cannot cancel the command on the host, so cancel-and-restart would race two
  `apply.sh` runs against one cluster.
- **D30 P2c closes on live evidence, gathered after merge:** the pull request
  can show only that the pipeline is well-formed. `TerraformPlan` on it is
  expected to exit 2 with the new resources — a no-op there would be the red
  flag — and the credentialed jobs do not run on a pull request at all. Phase
  close requires four artifacts, recorded on issue #49: an end-to-end deploy of
  a real `main` SHA, green in the Actions log; a rollback, demonstrated by
  dispatching the previous SHA and `smoke.sh` passing on it; a canary leak
  rehearsal, clean, performed **before** the channel first reads the real
  `.env` — the run reads an env file holding unique canary values, and the
  complete output (SSM stdout and stderr, and the Actions log) is swept for
  them, zero hits being the pass; and `TerraformPlan` a no-op (exit 0) after
  the owner's apply. EBS snapshot `snap-0f365806e0eaf9fb6`, retained past P2b
  as the floor under the first automated deploy (the #49 ruling), is deleted
  once that deploy is green and its rollback demonstrated, and the deletion is
  recorded with the evidence. *Rejected:* closing the slice on a green pull
  request; letting the first automated run touch the real `.env`; deleting the
  snapshot at P2b close. *Why:* a green pull request proves the pipeline
  parses, not that it deploys. The real `.env` holds the Postgres and Grafana
  credentials and the Actions log is public, so the first time this channel
  reads that file must not also be the first time its output hygiene (D23) is
  tested. The snapshot is the one rollback floor that does not depend on the
  pipeline under test.
> **Erratum (2026-09-27, P2c):** D13's drift-detection claim — every CI plan doubling as
> config-vs-reality drift detection — was unenforced from the P1 merge until P2c. The
> `hashicorp/setup-terraform` action installs a wrapper by default, and the wrapper maps
> Terraform's exit 2 (changes pending) to exit 0, so the `TerraformPlan` job's case dispatch
> never saw a non-zero code: every CI "no-op" in that window was unproven, and the drift
> alarm on `main` could not fire. Discovered by the P2c slice's expected-exit-2 check, when
> a fourteen-resource PR reported "no-op" — the impossible pass the P2a exit-semantics design
> had named in advance as the red flag. Fixed by `terraform_wrapper: false` on the plan job's
> setup step in the same PR. Owner-local `-detailed-exitcode` runs never used the wrapper and
> were unaffected; the P1 bootstrap and P2b evidence relied on local runs and stand.

> **Erratum (2026-09-28, P2c):** D30's canary leak rehearsal did not take place. No run sent the
> SSM channel an env file of canary values before it read the real one. Deploy run 36357281534
> (2026-09-27) was both the channel's first read of the host `.env` and the first end-to-end test
> of its output hygiene (D23), which is the ordering D30 rejected. Three things stand in for it.
> First, K3sSmoke runs the same `apply.sh` and `smoke.sh` against a synthetic `.env`, and their
> fixed-line output matches the SSM run line for line. Second, the public logs of runs
> 36357281534, 36517554863 and 36518530716 were swept after the fact for the four real values:
> zero hits. Third, those logs hold the complete SSM stdout and stderr, and every line in them
> comes from git, the AWS CLI, `k3s ctr` or a fixed `apply.sh`/`smoke.sh` line. **Residual gap:**
> (1) The clean result for the first read was established after the fact. Had it leaked, the
> credentials would have sat in a public log from about 23:04Z that day until the first sweep at
> no later than 2026-09-29T03:57Z, and the remedy would have been rotation, not prevention.
> (2) Every real run succeeded, so the channel's failure paths — the `die` lines, the kubectl
> stderr left unsuppressed on the namespace, ConfigMap and manifest applies, and SSM's
> Failed/TimedOut outcomes — have never carried real or canary values on any channel. Their
> hygiene rests on the code alone. (3) The synthetic canary seeds two of the four keys, leaving
> `SLACK_WEBHOOK_URL` empty and `GF_ADMIN_USER` unset, so CI takes the default-injection branch
> and the host takes the pass-through one. It is also not asserted: no CI step fails when a
> canary appears, and the evidence is a manual sweep of run 36356514105. (4) The public log was a
> complete record of SSM output on these three runs only because it fit (at most 1,053 stdout and
> 803 stderr characters, against deploy.yml's 80/40-line tails and SSM's 24,000/8,000-character
> limits). The pipeline does not guarantee that. The frozen text is not edited.

# Operator phase — ADR summary (D31–D37), v2

> FROZEN (owner rulings 2026-09-29 recorded in D31/D34). Decisions for the operator that the
> 2026-09-29 re-sequencing ruling places ahead of the P3 stretch (`docs/PHASE2.md`, P3 erratum;
> the contract is `docs/PHASE3.md`). The first draft of this block (v1) was gated APPROVED on
> direction, on ADR A — in-place adoption of the live api Deployment — and on ADR B — monorepo,
> `/operator`, and the pins — and REVISE on eight findings; all eight are integrated here, which
> makes this v2. Everything above is untouched; this block appends only.

- **D31 Scope, home and pins; the kind is `ServingDeployment`:** a Go operator in this
  repository at `/operator`, scaffolded with kubebuilder v4.15.0 — a scaffold-time tool, not a
  build dependency — on controller-runtime v0.24.x and k8s.io v0.36.x, the band matching D19's
  k3s `v1.36.4+k3s1`. It serves one namespaced CRD. The live `deployment/api` is adopted in
  place: the `ServingDeployment` becomes its owner through an ownerReference and the name is
  kept, and `20-api.yaml` loses its Deployment in the same slice that first applies the CR
  through `apply.sh`, so no tree both renders `deployment/api` from a manifest and hands it to
  the operator. The kustomize `config/` tree kubebuilder scaffolds is not kept: the CRD, the
  RBAC and the operator's own Deployment are hand-flattened into `deploy/k3s/manifests/`, the
  overlay is deleted, and a CI check regenerates them with controller-gen and fails on any
  diff. The kind was renamed at design review under `PRINCIPLES.md` §1 Law 1: the resource
  moves image tags, and model identity — `model_name`, `model_revision`, `model_version` — is
  frozen inside the image, not in the spec. *Provenance:* the owner brief's original name was
  `ModelDeployment`; the rename is owner-ruled 2026-09-29. *Rejected:* keeping
  `ModelDeployment` with a recorded caveat; a separate operator repository; keeping the
  kustomize tree beside the flattened manifests. *Why:* the operator's manifests land in
  `deploy/k3s/manifests/` and its changes gate the same k3d rehearsal (D35), which a second
  repository would split in two. The kustomize tree and the flattened manifests would be two
  copies of one description — the hazard D21 closed for the scrape config, two files that
  will disagree silently — and the sync check is what closes it here.
- **D32 Control flow — the pipeline sets the stable tag, a human opens the window:** the CR
  that `apply.sh` renders carries only `spec.imageTag`. `spec.canaryImageTag` and
  `spec.canaryReplicas` are set by a host-side `kubectl patch` and are never rendered. The rule
  is that a pipeline deploy during an open window closes it, and it holds mechanically: after
  the CR apply, `apply.sh` sends a constant merge patch — `canaryImageTag: null`,
  `canaryReplicas: 0` — which is a no-op when no window is open and prints a fixed output line
  when it closes one. `apply.sh` can close a window and can never open one. The deploy
  sequence is frozen: CRD apply → wait for `Established` → operator apply and rollout wait →
  CR apply → close-window patch → wait for the CR's `Ready` with
  `observedGeneration == generation` → `rollout status deployment/api` → `smoke.sh`. Promotion
  is ordered by the operator: the stable Deployment rolls to the promoted tag and completes
  while the canary is still serving, and only then does the canary go to 0, so there is no
  serving gap. *Rejected:* a server-side-apply field manager owning only the stable fields, so
  that a pipeline apply would leave an open window alone. *Why:* a window that survives a
  deploy survives onto a NEW stable, and the comparison it was opened for silently becomes a
  comparison against a different stable; and which manager owns which field would live in the
  cluster's `managedFields`, invisible to anyone reading the repository.
- **D33 The boundary claim, scoped; RBAC:** the AWS side is unchanged, and checkably so — no
  diff under `infra/ec2/`, with D24, D28 and D29 untouched. The in-cluster surface is new, and
  it is reviewed here. The operator holds one namespaced Role in `mlobs`: read (get, list,
  watch) and create, update, patch and delete on `apps/deployments`; `servingdeployments` with
  their `status` and `finalizers` subresources; create and patch on events; and
  `coordination.k8s.io` leases for leader election. It has no write on CRDs — those are
  applied by `apply.sh` under the host's admin credentials. It has no ClusterRole: metrics bind
  to localhost with authn/authz disabled in v0. There are no webhooks in v0; the CRD's OpenAPI
  schema carries the validation. *Rejected:* a ClusterRole for the scaffold's metrics
  authn/authz filter. *Why:* that filter calls the cluster-scoped TokenReview and
  SubjectAccessReview APIs, so it is the one thing in the scaffold that would need a
  cluster-wide grant, and v0 has no off-pod metrics consumer to justify it. The AWS half of the
  boundary claim is checkable from `infra/ec2/`, but an operator that writes Deployments is new
  privilege inside the cluster, so the claim is scoped to what it covers and the new surface is
  enumerated rather than implied.
- **D34 Traffic split — NodePort 8000 on an exact range:** the api Service becomes
  `type: NodePort` with `nodePort: 8000` and `externalTrafficPolicy: Local`, and k3s runs with
  `--service-node-port-range=8000-8000`, pinned exactly. The flag moves in the three pin sites
  D19's version already lives in (`docs/K3S.md`): the host install; the K3sSmoke k3d
  arguments, as `--k3s-arg '--service-node-port-range=8000-8000@server:*'`; and the local k3d
  recipe in `deploy/k3s/README.md`. A D19 erratum records the api's move from `hostPort` to
  NodePort; Grafana keeps `hostPort` 3000, and the security group is untouched. Three
  properties are stated in this ADR and in the README rather than left to be discovered: the
  split is by replica ratio; it is per connection, so a keep-alive client stays on one pod; and
  rollback is human-triggered in v0. Loopback `127.0.0.1:8000` is asserted in the rehearsal. It
  rides kube-proxy's iptables-mode `route_localnet`, so it is re-verified on any k3s bump. The
  canary is scraped through its own ClusterIP Service, `api-canary`, and the job `api_canary`
  joins the one scrape file (D21). The details are owner-ruled 2026-09-29 as specified here —
  the exact 8000-8000 range, `externalTrafficPolicy: Local`, the three-site flag pin and the
  three caveats. *Rejected:* the scrape-only fallback, in which the canary takes no live
  traffic and is observed only through its scrape. *Why:* D19 turned NodePort down because a
  port in 30000–32767 moves the port a stranger types. An exact range keeps that port at 8000
  — the one the security group already admits — while a Service, unlike a hostPort, can put
  two Deployments behind one port on one node.
- **D35 CI shape:** the operator's end-to-end tests run on the k3d image K3sSmoke already pins,
  `rancher/k3s:v1.36.4-k3s1`, not on kind, and the CR there names a stub image (`pause` or
  `http-echo`) rather than a torch build. envtest is pinned to 1.36.x. Leader election is
  tested as a lease-acquisition assertion only. `^operator/` joins the `K3sPaths` filter; the
  DocsGate sweeps extend to `operator/`; and Dependabot gains the `gomod` ecosystem at
  `/operator`, weekly, with an ignore constraint holding k8s.io at the v0.36 minor and
  controller-runtime at v0.24 — a band released only together with a k3s bump recorded as a
  D19 erratum. *Rejected:* kind; a timing-based failover demonstration. *Why:* kind would
  rehearse the operator against a cluster the host does not run, where the k3d image is the
  host's own pinned k3s (D19). A failover demo waits on lease durations and pod-kill timing,
  which makes it flaky by construction; that the lease is acquired is the part that can be
  asserted deterministically.
- **D36 Rollback across the boundary — ordered, and rehearsed before it is needed live:**
  undoing the operator is six steps, in order. (1) Canary to 0 by host patch. (2) Delete the
  CR. (3) Wait, to a bound, for `deployment/api` to be garbage-collected through its
  ownerReference — the proof that the adoption is undone. (4) Scale down or delete the
  operator; the CRD is deleted last or left inert. (5) Dispatch the pre-cutover SHA through
  the pipeline; that tree's `20-api.yaml` still carries the Deployment. (6) `smoke.sh` green.
  Steps 1–4 run outside the pipeline, as host `kubectl` over an SSM session, and are stated as
  such; steps 5–6 are the pipeline. The sequence is rehearsed green in k3d CI, with the stub
  image, before O4 runs it live. *Rejected:* deleting the CRD before the CR; "rolling back" the
  operator itself. *Why:* the CR is deleted while the operator still runs, and its deletion is
  the event step 3 waits on; removing the CRD first would take every CR with it, outside that
  order. The adoption lives in the ownerReference on `deployment/api`, not in the operator's
  version, so an older operator would still be reconciling an adopted api.
- **D37 State is enforced, not assumed:** the operator writes two conditions on the CR,
  `CanaryActive` and `ShadowPaused`, each with `lastTransitionTime`, and is their sole writer.
  An open window pauses the shadow scorer — two torch api pods plus the shadow would not fit
  under D26's bar on the 4GB host — so the stack is in exactly one of two states: steady
  (canary = 0 ∧ shadow = 1) or window (canary ≥ 1 ∧ shadow = 0). `smoke.sh` asserts
  exactly-one-of from cluster state. Its target-set assertion becomes state-aware: `api_canary`
  is required iff `CanaryActive`, `shadow_scorer` iff not `ShadowPaused`, and `drift_shadow` in
  both states — its count-based window merely ages while the shadow is paused.
  `prometheus/prometheus.yml` stays the one scrape description (D21), and the fixed workload
  lists in `apply.sh` and `smoke.sh` become state-aware with it. The window's TTL is 45
  minutes, and the arithmetic is recorded with it: `XADD MAXLEN ~ 50000` at the frozen 5 rps is
  a trim horizon of 10,000 s, about 2.8 h; the TTL stays at or below a third of
  50000 / observed rps and is recomputed before any higher-rate run; a shadow left paused past
  the horizon takes a permanent, silent gap.

> **D37 addendum (2026-09-29, O1):** the `Ready` condition joins the operator-written set — True
> iff the reconciled stable Deployment is Available and status.observedGeneration equals
> metadata.generation — supplying the wait target D32's deploy sequence names. Ruled at O1 start
> (issue #65); no frozen text edited.

> **D32 clarification (2026-09-29, O2):** the spec's writers stay exactly the frozen set — the
> pipeline renders `spec.imageTag`, a human host-side patch opens a window, `apply.sh`'s constant
> patch closes one — and the operator is never a third, expiry included. When the 45-minute TTL
> lapses (D37 addendum below) the operator restores the cluster to steady and latches the
> condition, leaving the stale canary fields in the spec: an expired-but-unclosed spec is a legal,
> steady-equivalent state, because `smoke.sh` judges from cluster state and conditions, never from
> the canary fields. The spec is normalized by the same constant close-window patch — the next
> pipeline deploy prints its fixed line, or a human runs the patch by hand. Until then the latch
> holds: editing the canary fields of an expired window does not reopen it; a new window requires
> the spec to pass through the closed shape first, which is what makes the TTL unevadable. Ruled
> at O2 start (issue #66); no frozen text edited.

> **D33 addendum (2026-09-29, O2):** D37's pause is the operator's to enforce, so the Role gains
> the minimum that makes it possible: get, list and watch on `apps/statefulsets` in mlobs, and
> patch on the `statefulsets/scale` subresource restricted by `resourceNames` to `shadow-scorer`.
> The shape is stated honestly: RBAC cannot name-scope list and watch, so the read grant covers
> the namespace's two StatefulSets, while the only write the operator holds on a StatefulSet is
> the replica count of the one named object, through the scale subresource only — and the
> controller writes only the literal values 0 and 1, asserted in test. It cannot touch the
> consumer, and it cannot touch the shadow scorer's template or image. The grant is expressed as
> `+kubebuilder:rbac` markers (controller-gen emits `resourceNames`) and lands in the generated
> half of `02-operator-rbac.yaml` under the existing sync check, which now also asserts the file
> contains no ClusterRole. D33's boundary is unchanged: one namespaced Role, no ClusterRole, no
> webhooks, no CRD write. Ruled at O2 start (issue #66); no frozen text edited.

> **D35 deviation (2026-09-29, O2):** the stub-image ruling meets a harder assertion than it
> anticipated. State-aware smoke in the window state proves the canary serves: `/predict` through
> the shared Service reaches the canary on roughly half the connections, and the `api_canary`
> target must be up — `pause` or `http-echo` can do neither. In K3sSmoke the CR's canary therefore
> names the api image the job already builds, under a second 40-hex tag on the same local build:
> no second torch build and no new pull, which is what the stub ruling actually protected. The
> stub stays exactly where its reasoning holds — the operator-only k3d e2e (leader election and
> reconcile mechanics, no smoke). O2's acceptance parenthetical "(k3d, stub image)" is read with
> this deviation. Ruled at O2 start (issue #66); no frozen text edited.

> **D37 addendum (2026-09-29, O2):** "an open window pauses the shadow scorer" names the operator
> as the actor — O1's committed condition reasons already read that way — and O2 gives it the
> grant (D33 addendum) and the protocol. The ordering is D26's bar made mechanical: on open, the
> shadow is scaled to 0 and observed gone before the canary comes up; on close or promote, the
> canary is observed gone before the shadow returns to 1; the stable-first half of promotion is
> already frozen in D32 and is not restated. `ShadowPaused` is computed from the StatefulSet
> actually observed, never from intent. The 45-minute TTL is operator-enforced: the clock is
> `CanaryActive`'s last False→True `lastTransitionTime` — persisted in status, so it survives an
> operator restart — the reconciler schedules itself to the deadline with `RequeueAfter`, and a
> mid-window `canaryImageTag` change does not restart it, because `CanaryActive` does not
> transition. At the deadline the operator restores steady and sets `CanaryActive` False with
> reason `WindowExpired`; the spec side is the D32 clarification above. With enforcement in place
> the manual shadow switch (`kubectl scale statefulset/shadow-scorer`) is retired: the operator is
> the sole writer of the shadow's scale from O2, a hand scale is reverted on the next reconcile,
> and the switch's documentation (README, `docs/K3S.md`, `deploy/k3s/README.md`, the
> 22-shadow-scorer.yaml comment) is updated in-slice; measured history stays as measured.
> `22-shadow-scorer.yaml` drops its `replicas: 1` line so a pipeline apply stops resetting the
> operator's scale, with the one-time caveat recorded: the first apply after the removal patches
> the field to null and the API server defaults it to 1 — benign, because a fresh cluster is
> steady and on a live one the same run's close-window patch and the operator's next reconcile
> re-enforce the order. O3's runbook still carries the TTL arithmetic and the out-of-pipeline
> steps, unchanged. Ruled at O2 start (issue #66); no frozen text edited.

> **D19 erratum (2026-09-29, O2):** "host ports kept" narrows to Grafana. The api moves from
> `hostPort` 8000 to the D34 NodePort Service on the exact 8000-8000 range; the port a stranger
> types is unchanged and the security group is untouched. The operator's api template carries no
> `hostPort`; the live host's adopted Deployment still does, and sheds it in a one-time
> out-of-pipeline patch sequenced inside O4's gated window (issue #68) — adoption stays
> ownerReference-and-image only. The api keeps `strategy: Recreate`: the reason was the hostPort
> bind deadlock and is now D26's memory bar — a surged second torch pod does not fit beside the
> shadow on the 4GB host.

> **D34 erratum (2026-09-29, O2):** the exact 8000-8000 range survives contact with k3s v1.36.4
> only without the bundled network-policy controller: it refuses a single-port range and k3s
> crash-loops. The ruled range stays exact; the controller is disabled instead —
> `--disable-network-policy` joins the range flag as an atomic pair everywhere the range is set.
> The in-repo pin sites gain the pair at O2; the third site, the live host's
> `/etc/rancher/k3s/config.yaml`, gains it inside O4's gated window (issue #68) with the rest of
> the cutover, deliberately deferred rather than missed. The cost is stated plainly: the stack
> defines no NetworkPolicy today, so nothing enforced is lost, but any NetworkPolicy added later
> would sit silently unenforced until the controller returns, and re-enabling it means revisiting
> the range. Verified against a real v1.36.4 cluster at O2 (issue #66, PR #81); no frozen text
> edited.

> **D30 addendum (2026-10-05, O4):** a pre-cutover EBS snapshot was considered at O4 and ruled
> out, per the O4 acceptance in `docs/PHASE3.md`; the reasoning is recorded in `docs/RUNBOOK.md`,
> "No EBS snapshot before the cutover (D30)" (issue #68). No frozen text edited.

> **D34 note (2026-10-05, O4):** the exact 8000-8000 range forecloses any staged or
> scratch-NodePort cutover path. No NodePort other than 8000 is admissible, so the api Service
> cannot land anywhere first, and 8000 is the live api's `hostPort` until the one-time patch (D19
> erratum above) takes it out. A cutover therefore necessarily has a dark window, from that patch
> until the dispatch's NodePort Service lands. Its cost is accepted and recorded, not engineered
> away: the 15-minute trip in `docs/RUNBOOK.md`'s abort criteria, which is the 30-minute dark
> ceiling less the longest way back on record (13 minutes, a floor on the worst case and not a
> bound). The live cutover stayed inside it, dark about 11 minutes. Owner-ruled 2026-10-02 in the
> review of O4's abort criteria, which merged with #87 (issue #68); no frozen text edited.

> **Incident (2026-10-05, O4):** #87's operator-image preflight made every pre-operator SHA
> undeployable, D36's way-back target `16a8c86` included: it checked `mlobs-operator` at every
> target SHA, and no operator image exists for a SHA that predates the operator. Found live at
> D36 step 5 — the dispatch of `16a8c86` (run 37276462319) failed the preflight before touching
> the host — and service was restored by a forward dispatch of `d30bcdf` (run 37276684801), dark
> about 10 minutes, inside the 15-minute trip. Fixed by #88, merged as `2554597`: the check is
> keyed on the tree, applying only when the target SHA ships `03-operator.yaml`, and a tree that
> ships it without a published image still fails. The fixed path then carried the completed live
> D36 re-run (run 37339612097). CI's `RollbackRehearsal` could not have caught it: it drives
> `apply.sh` below the pipeline, a gap `docs/RUNBOOK.md` documents ("What the rehearsal proves,
> and what it does not"). The pipeline half of D36's way back is therefore tested only live, and
> has exactly one completed live test on record: that re-run (issue #68); no frozen text edited.

# Phase 3 P3 (revised) — ADR summary (D38–D40)

> FROZEN (owner ruling 2026-10-05 recorded in D39). Decisions for the revised P3 stretch —
> Helm packaging of the operator, the ephemeral EKS demonstration, Compose retirement
> (`docs/PHASE2.md` P3 erratum; the contract is `docs/PHASE3.md`, slices H1–H3). The first
> draft of this block was gated FREEZE WITH AMENDMENTS on eight findings; all eight are
> integrated here, and the one item the gate left open — where the live day's de-scope valve
> sits — is owner-ruled 2026-10-05 and recorded in D39. Budget, honestly: H1 ≈ 1 weekend,
> H2 ≈ 1, H3 ≈ 0.5 — ≈ 2.5 against the roughly 1–2.5 weekends left of the 4–6 band, so the
> phase may close up to half a weekend over it; the owner accepts that rather than cut the
> live demonstration (the D39 ruling). Everything above is untouched; this block appends only.

- **D38 The chart covers the operator and nothing else; the flattened manifests stay the
  authority:** a Helm chart at `deploy/helm/mlobs-operator/` (version 0.1.0) packages exactly
  what `01-servingdeployment-crd.yaml`, `02-operator-rbac.yaml` and `03-operator.yaml` already
  describe: the CRD as a byte-exact copy under `crds/`, the RBAC and the operator Deployment
  as templates. Two value seams and no third: `image.prefix`, default `ghcr.io/muratalkan06`,
  and `image.tag`, no default, guarded in the template — `fail` unless the value passes
  `regexMatch "^[0-9a-f]{40}$"` — the schema's and the SSM document's pattern (D29). The
  namespace is not a value: a second guard fails any render where
  `ne .Release.Namespace "mlobs"`, and the pin is load-bearing twice — the binary's
  leader-election namespace is a literal and the D33 Role is shaped for `mlobs`, and the
  templates hard-code `metadata.namespace: mlobs` for byte-parity, so a stray `-n other` would
  split the objects from Helm's release bookkeeping. The flattened manifests stay
  authoritative: zero Helm in the k3s path, on the host or in the pipeline; the chart exists
  for D39's install and as the packaging evidence D17 deferred to P3. CI holds it to that. A
  `HelmParity` job renders the chart — `helm template -n mlobs`, a sentinel prefix, a 40-hex
  sentinel tag — against the flattened operator manifests with their `IMAGE_PREFIX` and
  `IMAGE_TAG` placeholders sed-rendered to the same sentinels, and fails on any diff; the diff
  is exact after stripping only `# Source:` lines, because the templates emit byte-identical
  manifests — no Helm-added labels, with `helm lint`'s recommended-label warnings accepted and
  stated. The parity render does not pass `--include-crds`: the CRD's parity is the sync
  check's job — `OperatorManifestSync` extends to the chart's `crds/` copy, byte-exact
  whole-file on both copies, three checked projections of one Go source. Helm's `crds/`
  contract is stated plainly: install-only — Helm never upgrades it and never deletes it — and
  this chart has no CRD upgrade path at 0.1.0; CRD evolution ships through the flattened
  manifests. Each guard carries one negative test with a fixed failure line (a non-40-hex tag;
  a non-`mlobs` release namespace); every render path — parity, lint, the e2e install — is
  invoked with `-n mlobs` and the sentinel values; `OperatorE2E` gains a chart-install phase;
  `deploy/helm/` joins the `K3sPaths` filter. The chart ships NO `ServingDeployment`: D32's
  writer set is frozen at three, and a chart-rendered CR would be a fourth. Helm itself is
  pinned to an exact 3.x patch — the 3.22 line, 3.22.0 current at this freeze — at both pin
  sites, CI and the owner script, and asserted on a fixed line in H2's evidence. Helm 4 is not
  adopted, and the cost is stated: 3.22 is the final Helm 3 minor, security-patched only into
  early 2027, so the pin is a renderer pin for byte-parity that outlives this phase by months,
  not years, and a Helm 4 move is a named revisit, never a silent drift. *Rejected:* the chart
  as authority, with the flattened manifests generated from it; a full-stack chart; the CR in
  the chart; `values.namespace`. *Why:* chart-as-authority inverts D31 days after its
  execution and reopens the two-descriptions hazard the sync check exists to close. A
  full-stack chart recreates the D21 copies — the scrape config and the provisioning files as
  chart data — that `apply.sh` builds from the canonical files precisely to avoid. The CR in
  the chart is the fourth spec writer D32 forbids. And `values.namespace` is multi-tenancy
  half-done: the D33 addendum's namespace-wide read grant stays deliberately closed, and
  single-namespace is recorded as the chart's non-goal rather than parameterised into a
  promise.
- **D39 The EKS demonstration — ephemeral, owner-run, identity named, bounded by time, and
  honest about what it shows:** `eksctl`, pinned exact (the 0.230.x line current at this
  freeze; the patch is fixed at H2 start and asserted on a fixed line) and installed with the
  k3d pattern — a pinned version fetched by the script, never "latest" — creates an ephemeral
  EKS cluster at Kubernetes 1.36, the D19/D35 band, verified available in standard support and
  re-verified, with the $0.10/h control-plane rate, on the demo day as an evidence line; in
  us-west-2, from a committed config at `deploy/eks/`: one `t3.medium` managed node, public
  subnets, `vpc.nat.gateway: Disable`, `withOIDC: false` and control-plane logging off — the
  last two pinned so the sweep's IAM OIDC-provider and CloudWatch log-group lines are
  expected-absent by construction. The cluster name carries a per-run nonce. The creating
  identity is named: a non-root admin IAM principal the owner designates at H2 start — EKS
  permanently binds cluster-creator admin, and root is ruled out by the repo's credential
  posture (D13, D24, D29); the script pins `AWS_PROFILE`, uses a dedicated scratch
  `--kubeconfig` deleted at teardown, and the evidence transcript OPENS with
  `aws sts get-caller-identity` on a fixed line. Terraform is rejected for this cluster, and
  `infra/ec2` with its state is untouched — asserted after teardown by
  `terraform -chdir=infra/ec2 plan` exiting 0. Every image is public GHCR (D18): the shadow
  never travels, no new S3 grant exists, D28 is frozen; the run's three GHCR pulls — the
  operator image, and the api image at both SHAs — are preflighted with deploy.yml's
  anonymous-token manifest check BEFORE cluster create (redis is the pinned public library
  image the manifest already names). The demo applies `00-namespace.yaml` and `11-redis.yaml`
  VERBATIM from `deploy/k3s/manifests` — reuse, not copies; the api's redis-ready
  initContainer needs redis — installs the chart, applies the CR (`40-servingdeployment.yaml`,
  rendered as `apply.sh` renders it) at a real `main` SHA, and the canary patch names a second
  real `main` SHA. The demonstrated sequence, on fixed lines plus kubectl outputs: operator
  Ready and the Lease held; `deployment/api` created and controlled at the rendered image,
  `Ready` at `observedGeneration == generation`; `/health` and `/predict` through
  port-forwards; a window opened; the canary observed serving; the constant close patch and
  steady again; then the D36-shaped teardown — CR delete, `deployment/api` garbage-collected
  through its ownerReference, `helm uninstall`, then the CRD deleted explicitly, since Helm's
  install-only `crds/` contract (D38) means uninstall never removes it — then
  `eksctl delete cluster --wait`, then the orphan sweep. `ShadowPaused` False with reason
  `ShadowNotFound` is recorded as expected: no shadow StatefulSet exists on EKS. **What
  "canary observed serving" means here, exactly:** on EKS neither `svc/api` nor
  `svc/api-canary` exists — `20-api.yaml` (since O2 the api Service) is excluded, its
  `nodePort: 8000` sits outside EKS's fixed NodePort range, and the operator's Role holds no
  Services grant (D33) — and a port-forward pins one pod, so D34's per-connection split
  structurally cannot be demonstrated on EKS; stated plainly, it remains proven by K3sSmoke
  and the live host. The claim is therefore pod-scoped: `kubectl port-forward
  deployment/api-canary`, a POST `/predict` returning 200 with `request_id`, `label` and
  `confidence`, and the canary's own `mlobs_predictions_total` incrementing across that
  forward; plus `CanaryActive` True at the current generation on the CR. `smoke.sh` does NOT
  run on EKS — its state classifier requires the shadow StatefulSet and the nine-workload
  stack — so the demo script carries its own fixed-line assertions, labelled as the demo's,
  not smoke's. Stated, not redemonstrated: the D34 split and NodePort are k3s properties; the
  45-minute TTL and the D26 bar are not re-run. The node arithmetic is recorded and complete:
  two api pods at 1Gi requests, the operator at 64Mi and redis at 32Mi sum to ≈2.1Gi, EKS
  system pods add ≈0.2Gi — ≈2.3Gi against a t3.medium's ~3.3Gi allocatable, which fits, and
  O4's live window measurement bounds real usage; `t3.large` is the one sanctioned deviation.
  **The enforced bound is time; the dollars are arithmetic:** the run is an owner-run local
  script, repeatable, NOT CI. If the live day runs long, the pre-decided de-scope line is the
  WINDOW SEGMENT: at T+2h from cluster-create the demo reduces to chart install → CR `Ready`
  at generation → stable serving through the port-forward → teardown and sweep. H1's
  chart-install e2e phase was considered as the valve and rejected — first contact with a
  chart install needs its rehearsal. Owner ruling at this freeze (2026-10-05): option (a) —
  the full scope stands, the overrun risk (at most half a weekend over the band) is accepted,
  and the T+2h window-segment cut is the pre-decided line. Independent of demo state, the
  script traps to teardown-first at a hard T+3h from create. The cost line reads "3h wall
  clock × verified rates, verified next day": ≈$0.145/h — the $0.10 control plane plus the
  t3.medium — under $0.50 expected, with $1 the expected-worst arithmetic, not a mechanism.
  **The orphan sweep, pinned:** it filters ONLY the three cluster-scoped tag families —
  `alpha.eksctl.io/cluster-name`, `eks:cluster-name`, `kubernetes.io/cluster/<name>` — and
  NEVER project-level tags: a `project=mlobs` sweep would enumerate the live P1 host, and the
  terraform exit-0 line above is the recorded backstop. It checks, each on a fixed line: EC2
  instances, with terminated-but-still-listed instances filtered out; security groups; ENIs —
  the DependencyViolation class; launch templates; CloudFormation stacks in terminal failure
  states; and the CloudWatch log-group and IAM OIDC-provider lines, expected-absent by
  construction. The evidence gains a T+24h line: the sweep re-run all-absent, plus a billing
  check, recorded. *Rejected:* a Terraform root for the ephemeral cluster; a workflow with a
  new OIDC role; the full stack on EKS; a LoadBalancer or NodePort exposure. *Why:* a second
  root means a second state for a cluster whose whole value is leaving nothing behind, and
  `eksctl create`/`delete` against a committed config is the pinned-tool pattern this repo
  already trusts in CI. A workflow needs an OIDC role that can create and destroy VPCs,
  clusters and instances — a grant surface out of all proportion to a demo, where D13 and D29
  hold the existing roles to read, plan and one fixed SSM document. The full stack on EKS
  re-demonstrates what K3sSmoke and the live host already prove, at torch-image cost, and the
  shadow cannot travel (D18, D28). And an exposure Service opens an inbound surface on a
  throwaway cluster for an audience of one — the port-forward is the honest shape of a demo
  whose only consumer is its operator.
- **D40 Compose retires in its own slice, with the sweep enumerated:** `docker-compose.yml`
  is deleted — the change D27 deferred to P3 and sized as its own slice — and the retirement
  is recorded with the last SHA that ships the file. Nothing functional breaks: no CI job and
  no script invokes Compose, verified by sweep; what remains is text, and the slice enumerates
  the textual class rather than trusting CI to find it. The nine manifest provenance headers
  ("Compose source of truth: the `<svc>` service in docker-compose.yml") are retargeted to
  "docker-compose.yml at <last-shipping-SHA>" — provenance kept, pointer made historical.
  `.env.example` stays — it documents the same gitignored `.env` that `apply.sh` reads — and
  its compose-up/-down instructions are retargeted, as are `sql/migrations/002_shadow.sql`'s
  Compose exec procedure and `docker/drift.Dockerfile`'s Compose-mount comment, each to its
  k3s-era equivalent. README's Quick start becomes the k3d recipe, pointing at
  `deploy/k3s/README.md`; the Compose-era Reproduce block is relabelled historical
  methodology; the measured Compose-era numbers keep their labels, byte-unchanged
  (`PRINCIPLES.md` §6 — the runtime is part of the methodology). `deploy/k3s/README.md`'s
  paragraph naming the Compose file the source of truth for what each service is, is updated:
  that truth now lives at the recorded SHA, not in the tree. D25's fallback-runtime clause
  ends with the file; the host's Docker engine is untouched. *Rejected:* retiring at O4;
  keeping the file dormant; deleting the Compose-era history or its numbers. *Why:* O4 was a
  gated live cutover, and bundling an unrelated deletion into it would have mixed two risks
  under one abort path — removing Compose is a change with its own risk and therefore its own
  slice, D27's words now executed. A dormant file is the D21 hazard in file form: a second
  description of the stack that no longer describes it, drifting silently under a header that
  still says "source of truth". And the history is measurement: deleting it would throw away
  the before-and-after that makes the migration legible, where relabelling keeps every number
  inseparable from its methodology.

> **D38 erratum (2026-10-05, H1):** Helm 3.22.0 reorders rendered documents by kind into install
> order and moves comment-only documents last, for any correct chart, so D38's "exact diff after
> stripping only `# Source:` lines" cannot come out empty as written. `HelmParity` compares the
> multiset of documents instead — each byte-exact, under a deterministic sort, stripping only
> Helm's own anchored Source lines on the chart side; the flattened file's own Source comment is
> kept and compared. Equal-or-stronger detection, reproduced independently at H1 (PR #94); no
> frozen text edited.
