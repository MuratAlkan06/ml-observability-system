"""The eight drift-job Prometheus metrics (docs/PLAN.md §5/§6), port 9109.

Label sets are pre-initialized so every frozen series is present from
first scrape.
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge

from .constants import ALL_TESTS

CLASS_CHI2_STAT = Gauge(
    "mlobs_drift_class_chi2_stat",
    "Chi-squared statistic of prediction-class distribution vs baseline (df=1).",
)
LENGTH_CHI2_STAT = Gauge(
    "mlobs_drift_length_chi2_stat",
    "Chi-squared statistic of token-length distribution vs baseline (df=4).",
)
CONFIDENCE_KL_NATS = Gauge(
    "mlobs_drift_confidence_kl_nats",
    "KL(P_window || Q_baseline) of confidence distribution, in nats.",
)
DRIFT_DETECTED = Gauge(
    "mlobs_drift_detected",
    "1 if the given drift test fired on the latest evaluated window, else 0.",
    ["test"],
)
WINDOW_SAMPLE_COUNT = Gauge(
    "mlobs_drift_window_sample_count",
    "Number of prediction rows in the latest sliding window (max 500).",
)
RUNS_TOTAL = Counter(
    "mlobs_drift_runs_total",
    "Drift evaluation loop iterations by outcome.",
    ["outcome"],
)
ALERTS_SENT_TOTAL = Counter(
    "mlobs_drift_alerts_sent_total",
    "Slack drift alerts successfully posted, by test.",
    ["test"],
)
LAST_RUN_TIMESTAMP = Gauge(
    "mlobs_drift_last_run_timestamp_seconds",
    "Unix timestamp of the most recent drift loop iteration.",
)

OUTCOME_EVALUATED = "evaluated"
OUTCOME_SKIPPED = "skipped_insufficient_samples"
OUTCOME_ERROR = "error"

for _outcome in (OUTCOME_EVALUATED, OUTCOME_SKIPPED, OUTCOME_ERROR):
    RUNS_TOTAL.labels(outcome=_outcome)
for _test in ALL_TESTS:
    DRIFT_DETECTED.labels(test=_test)
    ALERTS_SENT_TOTAL.labels(test=_test)
