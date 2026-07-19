"""Shadow metric registration + frozen names/buckets (v1.1 D4).

Touches each metric so a labeled series exists, then asserts the frozen exposed
names are present on the default registry and the confidence-delta histogram
carries its (negative-inclusive) frozen bucket edges.
"""

from __future__ import annotations

from prometheus_client import REGISTRY

from src.shadow_scorer import metrics


def test_all_shadow_metrics_registered():
    metrics.COMPARISONS.labels(primary_label="positive", shadow_label="negative").inc()
    metrics.PREDICTIONS.labels(label="positive").inc()
    metrics.CONFIDENCE_DELTA.observe(-0.15)
    metrics.INFERENCE_DURATION_SECONDS.observe(0.02)
    metrics.CONFIDENCE_RATIO.observe(0.8)
    metrics.EVENTS_SCORED.inc()
    metrics.ROWS_INSERTED.inc()
    metrics.DUPLICATES_SKIPPED.inc()
    metrics.EVENTS_DROPPED.inc()
    metrics.STREAM_LAG.set(3)
    metrics.PENDING_ENTRIES.set(1)

    def sv(name, labels=None):
        return REGISTRY.get_sample_value(name, labels or {})

    assert sv(
        "mlobs_shadow_comparisons_total",
        {"primary_label": "positive", "shadow_label": "negative"},
    ) is not None
    assert sv("mlobs_shadow_predictions_total", {"label": "positive"}) is not None
    assert sv("mlobs_shadow_confidence_delta_count") is not None
    assert sv("mlobs_shadow_inference_duration_seconds_count") is not None
    assert sv("mlobs_shadow_confidence_ratio_count") is not None
    assert sv("mlobs_shadow_events_scored_total") is not None
    assert sv("mlobs_shadow_rows_inserted_total") is not None
    assert sv("mlobs_shadow_duplicates_skipped_total") is not None
    assert sv("mlobs_shadow_events_dropped_total") is not None
    assert sv("mlobs_shadow_stream_lag_entries") == 3
    assert sv("mlobs_shadow_pending_entries") == 1


def test_confidence_delta_has_frozen_negative_buckets():
    # An observation of -0.15 lands in the (-0.2, -0.1] bucket: le="-0.1" counts it,
    # le="-0.2" and le="-0.4" do not — proving the negative edges are present.
    def bucket(le):
        return REGISTRY.get_sample_value("mlobs_shadow_confidence_delta_bucket", {"le": le})

    metrics.CONFIDENCE_DELTA.observe(-0.15)
    assert bucket("-0.4") is not None
    assert bucket("-0.2") is not None
    assert bucket("-0.1") is not None
    assert bucket("0.4") is not None
    # monotone: cumulative count at -0.1 strictly exceeds that at -0.2 for -0.15
    assert bucket("-0.1") > bucket("-0.2")
