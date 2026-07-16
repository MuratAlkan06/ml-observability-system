"""Window evaluation: all three drift tests per run (PLAN §5). Stdlib-only."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from datetime import datetime

from .baseline import CLASS_LABELS, Baseline
from .constants import (
    CLASS_CHI2_CRITICAL,
    CONFIDENCE_BIN_EDGES,
    CONFIDENCE_KL_CRITICAL,
    LENGTH_CHI2_CRITICAL,
    TOKEN_LEN_BIN_EDGES,
)
from .stats import chi2_statistic, fires, histogram_counts, kl_divergence_nats


@dataclass(frozen=True)
class WindowRow:
    """One row of the sliding window (SELECT label, token_count, confidence, ts)."""

    label: str
    token_count: int
    confidence: float
    ts: datetime


@dataclass(frozen=True)
class DriftResult:
    """Outcome of one evaluated run; maps 1:1 onto a drift_runs row (PLAN §4)."""

    window_start_ts: datetime
    window_end_ts: datetime
    sample_count: int
    class_chi2_stat: float
    class_drift: bool
    length_chi2_stat: float
    length_drift: bool
    confidence_kl_nats: float
    confidence_drift: bool
    drift_detected: bool
    bins: dict  # per-test observed-vs-expected diagnostics (JSONB column)


def evaluate_window(rows: Sequence[WindowRow], baseline: Baseline) -> DriftResult:
    """Run all three frozen tests on the window; any positive => drift_detected.

    (a) class chi-squared, df=1, fire > 6.635
    (b) token-length chi-squared over 5 frozen bins, df=4, fire > 13.277
    (c) confidence KL(P_window || Q_baseline) over 10 frozen bins, raw window
        probabilities (no smoothing), fire > 0.10 nats
    """
    n = len(rows)
    if n == 0:
        raise ValueError("evaluate_window requires a non-empty window")

    # (a) Class chi-squared: observed [n_neg, n_pos] vs expected n * class_probs.
    class_observed = [sum(1 for row in rows if row.label == label) for label in CLASS_LABELS]
    class_expected = [n * baseline.class_probs[label] for label in CLASS_LABELS]
    class_stat = chi2_statistic(class_observed, class_expected)
    class_drift = fires(class_stat, CLASS_CHI2_CRITICAL)

    # (b) Token-length chi-squared: 5 frozen bins vs n * token_len_probs.
    length_observed = histogram_counts([row.token_count for row in rows], TOKEN_LEN_BIN_EDGES)
    length_expected = [n * p for p in baseline.token_len_probs]
    length_stat = chi2_statistic(length_observed, length_expected)
    length_drift = fires(length_stat, LENGTH_CHI2_CRITICAL)

    # (c) Confidence KL: raw window histogram probs vs smoothed baseline probs.
    confidence_observed = histogram_counts([row.confidence for row in rows], CONFIDENCE_BIN_EDGES)
    confidence_window_probs = [count / n for count in confidence_observed]
    confidence_stat = kl_divergence_nats(confidence_window_probs, baseline.confidence_probs)
    confidence_drift = fires(confidence_stat, CONFIDENCE_KL_CRITICAL)

    timestamps = [row.ts for row in rows]
    return DriftResult(
        window_start_ts=min(timestamps),
        window_end_ts=max(timestamps),
        sample_count=n,
        class_chi2_stat=class_stat,
        class_drift=class_drift,
        length_chi2_stat=length_stat,
        length_drift=length_drift,
        confidence_kl_nats=confidence_stat,
        confidence_drift=confidence_drift,
        drift_detected=class_drift or length_drift or confidence_drift,
        bins={
            "class": {
                "categories": list(CLASS_LABELS),
                "observed": class_observed,
                "expected": class_expected,
            },
            "token_length": {
                "edges": list(TOKEN_LEN_BIN_EDGES),
                "observed": length_observed,
                "expected": length_expected,
            },
            "confidence": {
                "edges": list(CONFIDENCE_BIN_EDGES),
                "observed": confidence_observed,
                "expected": [n * p for p in baseline.confidence_probs],
            },
        },
    )
