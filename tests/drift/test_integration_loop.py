"""Drift loop integration tests against a THROWAWAY postgres (conftest fixture).

Covers the Appendix B S3 loop rows: <200 rows => skip with no drift_runs row;
>=200 drifted rows => evaluated run with correct booleans/stats/bins; empty
webhook => no alert attempt but the row is still written. Slack HTTP is
stubbed — no real network.

Local convenience: skips when psycopg/prometheus_client/docker are missing.
The CI job sets REQUIRE_DRIFT_INTEGRATION=1 which turns those skips into
hard failures (see conftest.skip_or_fail and the module guard below).
"""

import os
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest


def _import_or_skip(modname):
    try:
        return __import__(modname)
    except ImportError:
        if os.environ.get("REQUIRE_DRIFT_INTEGRATION") == "1":
            raise
        pytest.skip(f"{modname} not installed", allow_module_level=True)


_import_or_skip("psycopg")
_import_or_skip("prometheus_client")
_import_or_skip("pydantic_settings")

from prometheus_client import REGISTRY  # noqa: E402

from src.drift import runner  # noqa: E402
from src.drift.alerting import SlackAlerter  # noqa: E402
from src.drift.baseline import load_baseline  # noqa: E402
from src.drift.constants import MODEL_VERSION  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[2]
BASE_TS = datetime(2026, 7, 14, 12, 0, 0, tzinfo=timezone.utc)

INSERT_PREDICTION = """
INSERT INTO predictions
    (request_id, ts, text, token_count, label, confidence, model_version, latency_ms)
VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
"""


@pytest.fixture(scope="module")
def baseline():
    """The committed real baseline — integration runs end-to-end against it."""
    return load_baseline(REPO_ROOT / "baseline" / "baseline.json")


class RecordingPost:
    def __init__(self):
        self.calls = []

    def __call__(self, url, payload):
        self.calls.append((url, payload))


def seed_predictions(
    conn,
    n,
    *,
    label="negative",
    token_count=100,
    confidence=0.55,
    model_version=MODEL_VERSION,
    start_ts=BASE_TS,
):
    """Seed n rows; the defaults are heavily drifted vs the SST-2 baseline
    (all-negative labels, top token-length bin, bottom confidence bin)."""
    rows = [
        (
            str(uuid.uuid4()),
            start_ts + timedelta(seconds=i),
            "seeded drift-injection row",
            token_count,
            label,
            confidence,
            model_version,
            12.34,
        )
        for i in range(n)
    ]
    with conn.cursor() as cur:
        cur.executemany(INSERT_PREDICTION, rows)
    conn.commit()
    return rows


def metric(name, labels=None):
    return REGISTRY.get_sample_value(name, labels or {}) or 0.0


def drift_run_rows(conn):
    with conn.cursor() as cur:
        cur.execute(
            "SELECT sample_count, class_chi2_stat, class_drift,"
            " length_chi2_stat, length_drift, confidence_kl_nats,"
            " confidence_drift, drift_detected, alert_sent, bins,"
            " window_start_ts, window_end_ts FROM drift_runs ORDER BY id"
        )
        return cur.fetchall()


def test_below_guard_skips_and_writes_no_row(pg_conn, baseline):
    # 150 rows for our model + 400 rows for a foreign model_version: the
    # frozen window query filters on model_version, so the window is 150
    # (< 200) and the run must skip WITHOUT writing a drift_runs row.
    seed_predictions(pg_conn, 150)
    seed_predictions(
        pg_conn, 400, model_version="someone-elses-model", start_ts=BASE_TS + timedelta(hours=1)
    )
    post = RecordingPost()
    skipped_before = metric("mlobs_drift_runs_total", {"outcome": "skipped_insufficient_samples"})

    outcome = runner.run_once(pg_conn, baseline, SlackAlerter("https://hooks.example/x", post=post))

    assert outcome == "skipped_insufficient_samples"
    assert drift_run_rows(pg_conn) == []
    assert post.calls == []
    assert metric("mlobs_drift_runs_total", {"outcome": "skipped_insufficient_samples"}) == skipped_before + 1
    assert metric("mlobs_drift_window_sample_count") == 150


def test_drifted_window_evaluates_writes_row_and_alerts(pg_conn, baseline):
    seed_predictions(pg_conn, 500)  # all three tests must fire vs the real baseline
    post = RecordingPost()
    evaluated_before = metric("mlobs_drift_runs_total", {"outcome": "evaluated"})
    alerts_before = {
        test: metric("mlobs_drift_alerts_sent_total", {"test": test})
        for test in ("class", "token_length", "confidence")
    }

    outcome = runner.run_once(pg_conn, baseline, SlackAlerter("https://hooks.example/x", post=post))

    assert outcome == "evaluated"
    rows = drift_run_rows(pg_conn)
    assert len(rows) == 1
    (
        sample_count, class_stat, class_drift, length_stat, length_drift,
        kl_nats, confidence_drift, drift_detected, alert_sent, bins,
        window_start, window_end,
    ) = rows[0]

    assert sample_count == 500
    assert class_drift and class_stat > 6.635
    assert length_drift and length_stat > 13.277
    assert confidence_drift and kl_nats > 0.10
    assert drift_detected is True
    assert alert_sent is True
    assert set(bins) == {"class", "token_length", "confidence"}
    assert bins["token_length"]["observed"][4] == 500  # everything in the fat top bin
    assert window_start == BASE_TS
    assert window_end == BASE_TS + timedelta(seconds=499)

    # One Slack message per newly-firing test, frozen payload shape.
    assert len(post.calls) == 3
    texts = [payload["text"] for _, payload in post.calls]
    assert all(text.startswith("[mlobs] DRIFT: ") and "window_n=500" in text for text in texts)

    assert metric("mlobs_drift_runs_total", {"outcome": "evaluated"}) == evaluated_before + 1
    for test in ("class", "token_length", "confidence"):
        assert metric("mlobs_drift_alerts_sent_total", {"test": test}) == alerts_before[test] + 1
        assert metric("mlobs_drift_detected", {"test": test}) == 1.0
    assert metric("mlobs_drift_window_sample_count") == 500
    assert metric("mlobs_drift_class_chi2_stat") == pytest.approx(class_stat)
    assert metric("mlobs_drift_length_chi2_stat") == pytest.approx(length_stat)
    assert metric("mlobs_drift_confidence_kl_nats") == pytest.approx(kl_nats)
    assert metric("mlobs_drift_last_run_timestamp_seconds") > 0


def test_empty_webhook_writes_row_without_alert_attempt(pg_conn, baseline):
    seed_predictions(pg_conn, 500)
    post = RecordingPost()

    outcome = runner.run_once(pg_conn, baseline, SlackAlerter("", post=post))

    assert outcome == "evaluated"
    assert post.calls == []  # alerting disabled: zero HTTP attempts
    rows = drift_run_rows(pg_conn)
    assert len(rows) == 1
    alert_sent = rows[0][8]
    drift_detected = rows[0][7]
    assert drift_detected is True  # evaluation continued and detected drift
    assert alert_sent is False


def test_matching_window_evaluates_without_drift(pg_conn, baseline):
    # Sanity in the other direction: a window resampled FROM the baseline
    # itself must not fire (statistical self-consistency of the pipeline).
    import math
    import random

    rng = random.Random(42)
    labels = ["negative", "positive"]
    # Sample each characteristic independently from the baseline marginals.
    class_weights = [baseline.class_probs[label] for label in labels]
    token_reps = [5, 10, 20, 28, 100]  # one representative per frozen bin
    conf_reps = [0.52, 0.57, 0.62, 0.67, 0.72, 0.77, 0.82, 0.87, 0.92, 0.97]

    with pg_conn.cursor() as cur:
        rows = []
        for i in range(500):
            rows.append(
                (
                    str(uuid.uuid4()),
                    BASE_TS + timedelta(seconds=i),
                    "baseline-like row",
                    rng.choices(token_reps, weights=baseline.token_len_probs)[0],
                    rng.choices(labels, weights=class_weights)[0],
                    rng.choices(conf_reps, weights=baseline.confidence_probs)[0],
                    MODEL_VERSION,
                    12.34,
                )
            )
        cur.executemany(INSERT_PREDICTION, rows)
    pg_conn.commit()

    post = RecordingPost()
    outcome = runner.run_once(pg_conn, baseline, SlackAlerter("https://hooks.example/x", post=post))

    assert outcome == "evaluated"
    stored = drift_run_rows(pg_conn)
    assert len(stored) == 1
    drift_detected, alert_sent = stored[0][7], stored[0][8]
    assert drift_detected is False
    assert alert_sent is False
    assert post.calls == []
    assert math.isfinite(stored[0][5])  # KL finite: smoothing keeps q_i > 0
