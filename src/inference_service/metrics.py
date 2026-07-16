"""Prometheus metric inventory for the API (docs/PLAN.md §6, API table).

Registered on the default registry so the built-in ``process_*`` / ``python_*``
collectors are exposed alongside these. Counters are declared WITHOUT the
``_total`` suffix; prometheus-client appends it, yielding the frozen §6 names
(e.g. ``mlobs_http_requests_total``).
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram

# HTTP-level metrics. /metrics is intentionally excluded by the middleware.
HTTP_REQUESTS = Counter(
    "mlobs_http_requests",
    "HTTP requests to the API (/predict, /health).",
    ["endpoint", "method", "status"],
)
HTTP_REQUEST_DURATION_SECONDS = Histogram(
    "mlobs_http_request_duration_seconds",
    "End-to-end HTTP request duration in seconds.",
    ["endpoint"],
    buckets=(0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 1.0, 2.5),
)
HTTP_REQUESTS_IN_FLIGHT = Gauge(
    "mlobs_http_requests_in_flight",
    "In-flight /predict requests.",
)

# Inference-level metrics.
INFERENCE_DURATION_SECONDS = Histogram(
    "mlobs_inference_duration_seconds",
    "Model inference (tokenization + forward pass) duration in seconds.",
    buckets=(0.01, 0.02, 0.03, 0.04, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 1.0),
)
PREDICTIONS = Counter(
    "mlobs_predictions",
    "Predictions served, by label.",
    ["label"],
)
PREDICTION_CONFIDENCE_RATIO = Histogram(
    "mlobs_prediction_confidence_ratio",
    "Max-softmax prediction confidence (matches drift confidence bins).",
    buckets=(0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95),
)
MODEL_LOADED = Gauge(
    "mlobs_model_loaded",
    "1 if the model is loaded, else 0.",
    ["model_version"],
)

# Redis Streams producer metrics (§3 fire-and-forget contract).
STREAM_EVENTS_PUBLISHED = Counter(
    "mlobs_stream_events_published",
    "Prediction events successfully XADDed to the stream.",
)
STREAM_PUBLISH_FAILURES = Counter(
    "mlobs_stream_publish_failures",
    "Prediction events lost because XADD failed (fire-and-forget).",
)
