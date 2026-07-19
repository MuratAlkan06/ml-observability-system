"""Shadow scorer Prometheus metrics (v1.1 D4, frozen names/buckets).

Registered on the default registry so the built-in ``process_*`` / ``python_*``
collectors are exposed alongside these. Counters are declared WITHOUT the
``_total`` suffix; prometheus-client appends it, yielding the frozen names
(e.g. ``mlobs_shadow_comparisons_total``). Served on :9110.
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram

# Confidence buckets shared by delta/latency/ratio histograms (v1.1 frozen).
_CONFIDENCE_DELTA_BUCKETS = (-0.4, -0.2, -0.1, -0.05, -0.02, 0.02, 0.05, 0.1, 0.2, 0.4)
# Same buckets as the api's mlobs_inference_duration_seconds (overlay panels).
_INFERENCE_BUCKETS = (0.01, 0.02, 0.03, 0.04, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 1.0)
# Same 0.50-1.00 bins as mlobs_prediction_confidence_ratio / drift confidence bins.
_CONFIDENCE_RATIO_BUCKETS = (0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95)

# --- Comparison metrics (v1.1 D4) ---
COMPARISONS = Counter(
    "mlobs_shadow_comparisons",
    "Primary-vs-shadow comparisons (4-cell confusion matrix; agreement in PromQL).",
    ["primary_label", "shadow_label"],
)
CONFIDENCE_DELTA = Histogram(
    "mlobs_shadow_confidence_delta",
    "d = p_pos(shadow) - p_pos(primary) per event.",
    buckets=_CONFIDENCE_DELTA_BUCKETS,
)
INFERENCE_DURATION_SECONDS = Histogram(
    "mlobs_shadow_inference_duration_seconds",
    "Shadow model inference (tokenization + forward pass) duration in seconds.",
    buckets=_INFERENCE_BUCKETS,
)
PREDICTIONS = Counter(
    "mlobs_shadow_predictions",
    "Shadow predictions served, by label.",
    ["label"],
)
CONFIDENCE_RATIO = Histogram(
    "mlobs_shadow_confidence_ratio",
    "Shadow max-softmax prediction confidence (matches drift confidence bins).",
    buckets=_CONFIDENCE_RATIO_BUCKETS,
)

# --- Consumer-style health metrics (v1.1 D4) ---
EVENTS_SCORED = Counter(
    "mlobs_shadow_events_scored",
    "Stream events re-scored by the shadow model.",
)
ROWS_INSERTED = Counter(
    "mlobs_shadow_rows_inserted",
    "Rows actually written to the shadow_predictions table.",
)
DUPLICATES_SKIPPED = Counter(
    "mlobs_shadow_duplicates_skipped",
    "Valid events skipped by ON CONFLICT DO NOTHING (redelivery evidence).",
)
EVENTS_DROPPED = Counter(
    "mlobs_shadow_events_dropped",
    "Poison-pill entries dropped after exceeding the delivery-count limit.",
)
STREAM_LAG = Gauge(
    "mlobs_shadow_stream_lag_entries",
    "Consumer-group lag reported by XINFO GROUPS.",
)
PENDING_ENTRIES = Gauge(
    "mlobs_shadow_pending_entries",
    "Pending (unacked) entries reported by the XPENDING summary.",
)
