"""PostgreSQL access for the drift job: window query + drift_runs writes."""

from __future__ import annotations

import json

import psycopg
from psycopg.types.json import Jsonb

from .constants import MODEL_VERSION, WINDOW_SIZE
from .evaluate import DriftResult, WindowRow

# Frozen window query (PLAN §5): count-based latest-N, single secondary index
# idx_predictions_ts serves it.
WINDOW_QUERY = (
    "SELECT label, token_count, confidence, ts FROM predictions"
    f" WHERE model_version=%s ORDER BY ts DESC LIMIT {WINDOW_SIZE}"
)

INSERT_DRIFT_RUN = """
INSERT INTO drift_runs (
    window_start_ts, window_end_ts, sample_count,
    class_chi2_stat, class_drift,
    length_chi2_stat, length_drift,
    confidence_kl_nats, confidence_drift,
    drift_detected, alert_sent, bins
) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
"""


def connect(database_url: str) -> psycopg.Connection:
    return psycopg.connect(database_url)


def fetch_window(conn: psycopg.Connection) -> list[WindowRow]:
    """Latest 500 predictions for the frozen model version, newest first."""
    with conn.cursor() as cur:
        cur.execute(WINDOW_QUERY, (MODEL_VERSION,))
        return [
            WindowRow(label=label, token_count=token_count, confidence=confidence, ts=ts)
            for label, token_count, confidence, ts in cur.fetchall()
        ]


def insert_drift_run(conn: psycopg.Connection, result: DriftResult, *, alert_sent: bool) -> None:
    """Write exactly one drift_runs row for an evaluated run (skips write none)."""
    with conn.cursor() as cur:
        cur.execute(
            INSERT_DRIFT_RUN,
            (
                result.window_start_ts,
                result.window_end_ts,
                result.sample_count,
                result.class_chi2_stat,
                result.class_drift,
                result.length_chi2_stat,
                result.length_drift,
                result.confidence_kl_nats,
                result.confidence_drift,
                result.drift_detected,
                alert_sent,
                Jsonb(result.bins, dumps=json.dumps),
            ),
        )
    conn.commit()
