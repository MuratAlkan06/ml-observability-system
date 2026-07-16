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
