"""Consumer Prometheus metrics (docs/PLAN.md §6 consumer row).

Names, types and histogram buckets are frozen by the spec. Default
process/platform collectors stay enabled (start_http_server default registry).
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram

EVENTS_CONSUMED = Counter(
    "mlobs_consumer_events_consumed_total",
    "Stream entries read from the consumer group (new deliveries + reclaims).",
)
ROWS_INSERTED = Counter(
    "mlobs_consumer_rows_inserted_total",
    "Rows actually written to the predictions table.",
)
DUPLICATES_SKIPPED = Counter(
    "mlobs_consumer_duplicates_skipped_total",
    "Valid entries skipped by ON CONFLICT DO NOTHING (redelivery evidence).",
)
EVENTS_DROPPED = Counter(
    "mlobs_consumer_events_dropped_total",
    "Poison-pill entries dropped after exceeding the delivery-count limit.",
)
STREAM_LAG = Gauge(
    "mlobs_consumer_stream_lag_entries",
    "Consumer-group lag reported by XINFO GROUPS.",
)
PENDING_ENTRIES = Gauge(
    "mlobs_consumer_pending_entries",
    "Pending (unacked) entries reported by the XPENDING summary.",
)
BATCH_DURATION = Histogram(
    "mlobs_consumer_batch_duration_seconds",
    "Time to persist and acknowledge one batch of entries.",
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0),
)
