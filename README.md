# ML Observability System

[![CI](https://github.com/MuratAlkan06/ml-observability-system/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/MuratAlkan06/ml-observability-system/actions/workflows/ci.yml)
[![Python 3.12](https://img.shields.io/badge/python-3.12-blue.svg)](https://www.python.org/downloads/release/python-3120/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Real-time ML observability for a self-hosted sentiment model, treated like production
infrastructure. A FastAPI service runs DistilBERT (SST-2 sentiment) inference and streams
every prediction through Redis Streams into PostgreSQL; a drift job continuously compares the
latest-500-prediction window against a frozen baseline using three statistical tests — class
χ², token-length χ², and confidence KL divergence — and alerts to Slack when the input
distribution shifts. Request latency, pipeline throughput, and drift scores are all exported to
Prometheus and visualized in Grafana, and a host-side traffic simulator with a `--mode drift`
switch can trip all three detectors on demand. A second, smaller **candidate model** scores the
same live traffic in a shadow deployment, so agreement, confidence deltas, latency, and per-model
drift can be compared side by side to support a data-driven promote-or-hold decision — with zero
impact on the primary prediction path. It is a compact, end-to-end demonstration of the
observability that real ML systems need but rarely ship with.

## Architecture

```mermaid
flowchart LR
    sim[["Traffic simulator<br/>(host · --mode drift)"]] -->|POST /predict| api["FastAPI inference<br/>DistilBERT SST-2 (primary)"]
    api -->|XADD mlobs:predictions| redis[("Redis Streams")]
    redis -->|group pg_writer| consumer["Consumer<br/>(at-least-once)"]
    redis -->|group shadow_scorer| shadow["Shadow scorer<br/>MiniLM-L6 SST-2 (candidate)"]
    consumer -->|INSERT predictions| pg[("PostgreSQL")]
    shadow -->|INSERT shadow_predictions| pg
    drift["Drift job<br/>(primary)"] -->|window: predictions| pg
    drifts["Drift job<br/>(shadow)"] -->|window: shadow_predictions| pg
    drift -->|drift scores| prom[("Prometheus")]
    drifts -->|drift scores| prom
    drift -->|threshold breach| slack[["Slack webhook"]]
    drifts -->|threshold breach| slack
    api -->|/metrics| prom
    consumer -->|/metrics| prom
    shadow -->|/metrics| prom
    prom --> graf["Grafana dashboards"]
```

All services run under Docker Compose on a single node. Only the API (`:8000`) and Grafana
(`:3000`) are published for normal use; Prometheus (`:9090`) is bound to loopback for local
debugging, and the consumer (`:9108`), drift (`:9109`), and shadow-scorer (`:9110`) metrics
endpoints — plus the second `drift-shadow` job — are scraped over the internal Compose network and
never published to the host. The shadow scorer joins the same `mlobs:predictions` stream with its
**own consumer group**, so it never touches the primary prediction path.

## Demo

![Drift detection firing on the live Grafana dashboard](docs/assets/drift-demo.gif)

Healthy traffic keeps all three drift statistics well below their thresholds. Switching the
simulator to `--mode drift` saturates the sliding window with out-of-distribution reviews, and
within one 60-second evaluation cycle the class χ², token-length χ², and confidence-KL panels
spike past their thresholds while the **Drift detected** panel flips on and drift runs switch
from *skipped* to *evaluated*.

## Quick start

Prerequisites: Docker + Docker Compose, and Python 3.12 on the host for the traffic simulator.

```bash
# 1. Clone
git clone https://github.com/MuratAlkan06/ml-observability-system.git
cd ml-observability-system

# 2. Configure secrets (never committed — .env is gitignored)
cp .env.example .env
#   Edit .env and set at minimum:
#     POSTGRES_PASSWORD   (any strong value; applied at first initdb)
#     GF_ADMIN_PASSWORD   (Grafana admin; the stack refuses to start if unset)
#   Optional: SLACK_WEBHOOK_URL (empty disables alerting), GF_ADMIN_USER.

# 3. Build and start the stack (the shadow scorer + second drift job run by
#    default — the model-comparison feature populates out of the box)
docker compose up -d --build

#    Latency A/B "off" switch — stop the shadow scorer to measure /predict with
#    and without it (see "v1.1 load test" below); the primary path is unaffected:
#      docker compose stop shadow-scorer   # (docker compose start shadow-scorer to resume)

# 4. Drive traffic from the host (simulator needs only httpx)
python -m venv .venv && . .venv/bin/activate
pip install httpx
python -m src.simulator --mode normal          # healthy baseline traffic
python -m src.simulator --mode drift            # trips all three drift tests
#   Useful flags: --rate <rps> (default 5), --count <N> (default: run until Ctrl-C).
```

Then look at:

| Where | URL | Notes |
| --- | --- | --- |
| Grafana dashboards | http://localhost:3000 | Anonymous **Viewer** — no login. *mlobs — API & Inference*, *mlobs — Pipeline & Drift*, and *mlobs — Model Comparison*. |
| API docs (Swagger) | http://localhost:8000/docs | `POST /predict`, `GET /health`, `GET /metrics`. |
| Prometheus | http://localhost:9090 | Loopback-only (SSH-tunnel off-box). |

## Load test

Measured **on the deployed EC2 t3.medium** (2 vCPU, us-west-2, Ubuntu 24.04, Docker Compose,
single uvicorn worker) with [`hey`](https://github.com/rakyll/hey) `0.1.5` run on-instance over
loopback — 15 s warm-up to prime the model, then 60 s measured runs of a 26-token
positive-review payload:

> **≈14.7 req/s sustained, p95 85 ms on EC2 t3.medium — 0 errors.**

| Concurrency | Throughput | p50 | p95 | p99 | Errors |
| --- | --- | --- | --- | --- | --- |
| 1 | 14.72 req/s | 63.5 ms | 84.6 ms | 185.9 ms | 0 |
| 2 | 14.74 req/s | 126.7 ms | 197.9 ms | 390.6 ms | 0 |
| 4 | 14.77 req/s | 254.3 ms | 331.6 ms | 691.1 ms | 0 |

Throughput is flat across c=1/2/4 while latency scales linearly — the single-worker, CPU-only
inference path is the ceiling, so added concurrency just queues. CPU held ~66% avg / 86% max
during the runs. The instance ran in **unlimited CPU-credit mode**, so these reflect full burst
rather than a throttled t3 baseline.

For reference, the same methodology run **locally** on an Apple M4 Pro (Docker Desktop, 14-vCPU
VM) sustains **28.8 req/s at p95 37 ms** (concurrency 1, `torch.set_num_threads(2)`, 0 errors) —
roughly 2× the EC2 throughput, as expected from the wider host.

Reproduce (short warm-up primes the model, then the measured 60-second run; vary `-c` for
concurrency):

```bash
PAYLOAD='{"text":"A thrilling, moving, and beautifully acted film."}'
# warm-up
hey -z 15s -c 1 -m POST -T "application/json" -d "$PAYLOAD" http://localhost:8000/predict
# measured
hey -z 60s -c 1 -m POST -T "application/json" -d "$PAYLOAD" http://localhost:8000/predict
```

### v1.1 load test (shadow on/off)

> **Pending EC2 certification.** The headline v1.1 claim — that re-scoring every prediction with
> the shadow model adds **zero measurable latency to the primary `/predict` path** — is certified
> by re-running the `hey` methodology above on the deployed t3.medium with the shadow scorer **on**
> vs. **off** (`docker compose stop shadow-scorer`) and comparing p95. By design the shadow scorer
> reads `/predict`'s output *asynchronously* off the Redis stream, so it cannot sit in the request
> path; this run confirms that empirically under load. The certified numbers land here after the
> EC2 session; a local indicative run is captured in the slice's e2e evidence and is explicitly
> *not* the certified figure.

## How drift detection works

The drift job wakes every **60 s**, reads the **latest 500** predictions for the current model
version, and — if the window has at least **200** samples — runs three independent tests against
the frozen `baseline/baseline.json` (built from the 872-row SST-2 validation split). Any single
positive result marks the run as drift-detected:

| Test | Statistic | Fires when |
| --- | --- | --- |
| Class balance | χ², df = 1 | stat > **6.635** |
| Token-length distribution | χ² over 5 frozen bins, df = 4 | stat > **13.277** |
| Confidence distribution | KL(window ‖ baseline) over 10 frozen bins | > **0.10 nats** |

Critical values are hard-coded at α = 0.01 (no SciPy/NumPy — the math is pure Python). Each run
is persisted to the `drift_runs` table and exported to Prometheus; when `SLACK_WEBHOOK_URL` is
set, a breach posts to Slack (with a 15-minute per-test cooldown). The host simulator ships two
corpora selected by `--mode`: `normal` traffic tracks the baseline on all three axes and fires
nothing, while `drift` traffic is engineered to trip all three tests simultaneously once it
saturates the window.

## Shadow deployment & model comparison

Every prediction event on `mlobs:predictions` already carries the raw review text **and** the
primary model's label, confidence, and latency. A second container — the **shadow scorer** —
joins that same stream with its own consumer group (`shadow_scorer`, starting at `$` so it scores
from deploy forward), so a single message hands it both the scoring input and the primary half of
every comparison. It re-scores the text with a smaller candidate model, writes the result to a
dedicated `shadow_predictions` table (with the primary label/confidence denormalized in for
join-free agreement queries), and exports comparison metrics on `:9110`. Because it reads the
stream **asynchronously, off the request path**, a shadow crash or backlog can never add latency
to — or take down — the primary `/predict` path; the lag is observable on the dashboard and drains
at roughly 7–8× the arrival rate once the scorer recovers.

**Candidate model:** `philschmid/MiniLM-L6-H384-uncased-sst2` (`minilm-sst2-v1`) — a 6-layer,
H384 MiniLM (~22.7M params, **91 MB** fp32) fine-tuned on SST-2. On the same 872-sentence SST-2
validation split it scores **90.1% dev accuracy** versus **≈91.3%** for the primary DistilBERT, so
the question it lets you ask is: *can we serve a ~4× cheaper model without materially changing what
users see?* It ships no `id2label`, so the label map `LABEL_0→negative, LABEL_1→positive` is frozen
and asserted with two sanity probes at Docker build time; the shadow computes token counts with its
**own** tokenizer and builds its own drift baseline (`baseline/baseline-minilm.json`).

The **mlobs — Model Comparison** dashboard turns the two streams into a promote-or-hold picture:
agreement ratio over time and a 4-cell confusion matrix (from `mlobs_shadow_comparisons_total`),
a primary-vs-shadow latency p50/p95 overlay, the confidence-delta distribution
(`d = p_pos(shadow) − p_pos(primary)`), shadow pipeline health (lag, pending, and scored/inserted/
dropped rates), and both models' drift side by side. Drift runs **independently per model**: a
second drift job (`drift-shadow`) evaluates the `shadow_predictions` window against the candidate's
own baseline and exports the same metrics under a `drift_shadow` Prometheus job — so the two v1
dashboards pin their panels to `job="drift"` and the comparison dashboard shows both models. Slack
alerts are prefixed `[mlobs][<model_version>]`, so a firing alert names the model that drifted.

### Promotion decision

The dashboards exist to answer one question — *promote the candidate, or hold?* — against explicit
criteria:

- **Agreement** with the primary model holds at or above **~0.90** on in-domain traffic (proposal
  threshold), with no lopsided failure mode in the confusion matrix (e.g. the candidate flipping
  one class far more than the other).
- **No candidate-only drift:** the shadow drift job is not firing while the primary stays quiet —
  i.e. the candidate is not uniquely sensitive to the live distribution.
- **A latency advantage** that justifies the swap: the p50/p95 overlay shows the candidate is
  meaningfully cheaper, with no behavioral regression that outweighs it.

The critical caveat: this pipeline has **no ground-truth labels on live traffic**, so *agreement
measures behavioral delta, not correctness*. A 0.91 agreement means the candidate matches the
primary 91% of the time — not that either model is 91% right. The promotion verdict therefore
**triangulates** three independent signals: the **offline dev-set accuracy** (872-row SST-2: 91.3%
primary vs 90.1% candidate — the one place with real labels), the **live agreement plus
confidence/latency deltas** (how differently, and how much more cheaply, the candidate behaves on
production traffic), and **independent per-model drift** (whether either model is being fed OOD
input). Live agreement alone can never certify a model; it only tells you whether swapping it would
visibly change outputs.

**Next step (out of scope for v1.1):** the natural follow-up this evidence gates is a **canary** —
routing a small percentage of real `/predict` traffic to the candidate once agreement, drift, and
latency clear the bar above. Shadow scoring is the safe, zero-user-impact precondition for that
rollout; canary routing, automated promotion, and A/B significance testing are deliberately
deferred.

## Stack

| Layer | Technology |
| --- | --- |
| Inference API | FastAPI (single uvicorn worker) |
| Primary model | DistilBERT SST-2, baked into the image; `tokenizers==0.22.2` pinned |
| Candidate model (shadow) | MiniLM-L6-H384 SST-2, baked into the shadow image; scored off a second consumer group |
| Event stream | Redis Streams (`mlobs:predictions`; groups `pg_writer` + `shadow_scorer`) |
| Storage | PostgreSQL 16 (`predictions` + `shadow_predictions`) |
| Drift detection | Pure-Python χ² + KL against a frozen baseline, per model (`drift` / `drift-shadow`) |
| Metrics | Prometheus |
| Dashboards | Grafana (anonymous Viewer) |
| Orchestration | Docker Compose |
| Language | Python 3.12 |

## Roadmap

Built in waves of independently reviewable slices.

- [x] **Wave 1 — A · Reset & scaffold** — legacy stubs removed, frozen plan adopted, CI + tooling in place

**Wave 2 — parallel slices**
- [x] **S1 · Inference service** — FastAPI app, self-hosted model load, `POST /predict`, `GET /health`, `GET /metrics`
- [x] **S2 · Event pipeline** — Redis Streams producer → consumer group → PostgreSQL, at-least-once with idempotent writes
- [x] **S3 · Drift detection** — frozen baseline vs sliding window, Chi-squared + KL divergence, Prometheus export, Slack alerting
- [x] **S4 · Simulator + dashboards** — host-side traffic generator with drift injection, provisioned Grafana dashboards

**Wave 3**
- [x] **S5 · End-to-end + load test** — full integration demo (above) and local load test
- [x] **S5 · Deploy** — single-node EC2 t3.medium (Docker Compose, Ubuntu 24.04); only `:8000`
  and `:3000` exposed, IMDSv2 enforced. Live-verified on the instance: exactly-once pipeline at
  ~4.7k predictions and all three drift tests firing real Slack alerts.

**v1.1 — Shadow / candidate comparison**
- [x] **S6 · Shadow scorer** — MiniLM-L6 candidate re-scores live traffic off a second consumer
  group; `shadow_predictions` table; comparison metrics on `:9110`, zero primary-path impact
- [x] **S7 · Multi-model drift** — per-model drift jobs (`drift` / `drift-shadow`), model-scoped
  baselines and `[mlobs][<model_version>]` Slack prefixes
- [x] **S8 · Comparison observability** — *mlobs — Model Comparison* dashboard, promotion-decision
  criteria with the no-ground-truth caveat, docs
- [ ] **EC2 re-certification** — redeploy + shadow-on/off `hey` load test to certify zero
  primary-path latency impact under load, then tag `v1.1.0`

## Frozen specification

The complete frozen v1 and v1.1 design — scope contracts, HTTP/event/DB schemas, drift spec,
Prometheus metric inventory, shadow-comparison architecture, and verification matrix — lives in
[`docs/PLAN.md`](docs/PLAN.md). Implementation slices build against it verbatim.

## License

[MIT](LICENSE)
