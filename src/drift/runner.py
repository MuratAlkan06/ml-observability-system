"""Long-lived drift evaluation loop (PLAN §5): evaluate every 60s, never crash."""

from __future__ import annotations

import logging
import time

from prometheus_client import start_http_server

from . import db, metrics
from .alerting import SlackAlerter
from .baseline import Baseline, load_baseline
from .config import DriftSettings
from .constants import (
    EVAL_INTERVAL_SECONDS,
    METRICS_PORT,
    MIN_WINDOW_SAMPLES,
    TEST_CLASS,
    TEST_CONFIDENCE,
    TEST_TOKEN_LENGTH,
)
from .evaluate import evaluate_window

logger = logging.getLogger(__name__)


def run_once(conn, baseline: Baseline, alerter: SlackAlerter) -> str:
    """One evaluation cycle against an open connection; returns the outcome label.

    Skipped runs (< 200 rows) write NO drift_runs row; evaluated runs write
    exactly one row, with alert_sent=true iff >= 1 Slack message actually
    posted for the run.
    """
    rows = db.fetch_window(conn)
    metrics.WINDOW_SAMPLE_COUNT.set(len(rows))

    if len(rows) < MIN_WINDOW_SAMPLES:
        logger.info(
            "window has %d rows (< %d): skipping evaluation", len(rows), MIN_WINDOW_SAMPLES
        )
        metrics.RUNS_TOTAL.labels(outcome=metrics.OUTCOME_SKIPPED).inc()
        metrics.LAST_RUN_TIMESTAMP.set_to_current_time()
        return metrics.OUTCOME_SKIPPED

    result = evaluate_window(rows, baseline)
    sent = alerter.send_alerts(result)
    for test in sent:
        metrics.ALERTS_SENT_TOTAL.labels(test=test).inc()
    db.insert_drift_run(conn, result, alert_sent=bool(sent))

    metrics.CLASS_CHI2_STAT.set(result.class_chi2_stat)
    metrics.LENGTH_CHI2_STAT.set(result.length_chi2_stat)
    metrics.CONFIDENCE_KL_NATS.set(result.confidence_kl_nats)
    metrics.DRIFT_DETECTED.labels(test=TEST_CLASS).set(int(result.class_drift))
    metrics.DRIFT_DETECTED.labels(test=TEST_TOKEN_LENGTH).set(int(result.length_drift))
    metrics.DRIFT_DETECTED.labels(test=TEST_CONFIDENCE).set(int(result.confidence_drift))
    metrics.RUNS_TOTAL.labels(outcome=metrics.OUTCOME_EVALUATED).inc()
    metrics.LAST_RUN_TIMESTAMP.set_to_current_time()

    logger.info(
        "evaluated window n=%d class_chi2=%.4f length_chi2=%.4f confidence_kl=%.4f "
        "drift_detected=%s alert_sent=%s",
        result.sample_count,
        result.class_chi2_stat,
        result.length_chi2_stat,
        result.confidence_kl_nats,
        result.drift_detected,
        bool(sent),
    )
    return metrics.OUTCOME_EVALUATED


def run_forever(settings: DriftSettings | None = None) -> None:
    """Service entrypoint: load baseline, expose metrics on :9109, loop every 60s."""
    settings = settings or DriftSettings()
    baseline = load_baseline(settings.baseline_path)
    logger.info(
        "loaded baseline %s (sample_count=%d, created_at=%s)",
        settings.baseline_path,
        baseline.sample_count,
        baseline.created_at,
    )
    if not settings.slack_webhook_url:
        logger.info("SLACK_WEBHOOK_URL empty: alerting disabled, evaluation continues")
    alerter = SlackAlerter(settings.slack_webhook_url)

    start_http_server(METRICS_PORT)
    logger.info("drift metrics exposed on :%d", METRICS_PORT)

    while True:
        try:
            with db.connect(settings.database_url) as conn:
                run_once(conn, baseline, alerter)
        except Exception:
            logger.exception("drift evaluation cycle failed")
            metrics.RUNS_TOTAL.labels(outcome=metrics.OUTCOME_ERROR).inc()
            metrics.LAST_RUN_TIMESTAMP.set_to_current_time()
        time.sleep(EVAL_INTERVAL_SECONDS)
