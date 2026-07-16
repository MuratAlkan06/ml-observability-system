"""Shared fixtures for the drift test suite (docs/PLAN.md §5, Appendix B S3).

- Pure-math/baseline/alerting tests need only the stdlib + httpx (dev.txt).
- Integration tests get a THROWAWAY postgres:16-alpine via `docker run`
  (never S2's compose service) with the PLAN §4 DDL applied inline.
- Local convenience: integration prerequisites missing => tests SKIP.
  In CI the dedicated job sets REQUIRE_DRIFT_INTEGRATION=1, which turns
  every such skip into a hard failure so the tests cannot silently vanish.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys
import time
import uuid
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

REQUIRE_INTEGRATION = os.environ.get("REQUIRE_DRIFT_INTEGRATION") == "1"

# Verbatim copy of the PLAN §4 DDL for the two tables the drift job touches
# (sql/init.sql belongs to S2 and is deliberately not read here).
PLAN_S4_DDL = """
CREATE TABLE predictions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id    UUID             NOT NULL UNIQUE,
    ts            TIMESTAMPTZ      NOT NULL,
    inserted_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    text          TEXT             NOT NULL CHECK (char_length(text) BETWEEN 1 AND 1000),
    token_count   SMALLINT         NOT NULL CHECK (token_count BETWEEN 3 AND 256),
    label         TEXT             NOT NULL CHECK (label IN ('positive', 'negative')),
    confidence    DOUBLE PRECISION NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    model_version TEXT             NOT NULL,
    latency_ms    DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0.0)
);
CREATE INDEX idx_predictions_ts ON predictions (ts DESC);

CREATE TABLE drift_runs (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    window_start_ts    TIMESTAMPTZ      NOT NULL,
    window_end_ts      TIMESTAMPTZ      NOT NULL,
    sample_count       INTEGER          NOT NULL CHECK (sample_count > 0),
    class_chi2_stat    DOUBLE PRECISION NOT NULL,
    class_drift        BOOLEAN          NOT NULL,
    length_chi2_stat   DOUBLE PRECISION NOT NULL,
    length_drift       BOOLEAN          NOT NULL,
    confidence_kl_nats DOUBLE PRECISION NOT NULL,
    confidence_drift   BOOLEAN          NOT NULL,
    drift_detected     BOOLEAN          NOT NULL,
    alert_sent         BOOLEAN          NOT NULL DEFAULT FALSE,
    bins               JSONB            NULL
);
CREATE INDEX idx_drift_runs_run_at ON drift_runs (run_at DESC);
"""


def skip_or_fail(reason: str) -> None:
    """Skip locally; fail hard when the CI integration job demands execution."""
    if REQUIRE_INTEGRATION:
        pytest.fail(f"REQUIRE_DRIFT_INTEGRATION=1 but {reason}", pytrace=False)
    pytest.skip(reason)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.fixture(scope="session")
def pg_dsn():
    """Throwaway postgres:16-alpine container with the PLAN §4 tables applied."""
    try:
        import psycopg
    except ImportError:
        skip_or_fail("psycopg is not installed")
    if shutil.which("docker") is None:
        skip_or_fail("docker CLI is not available")

    port = _free_port()
    name = f"mlobs-drift-it-{uuid.uuid4().hex[:8]}"
    run = subprocess.run(
        [
            "docker", "run", "--rm", "-d", "--name", name,
            "-e", "POSTGRES_USER=mlobs",
            "-e", "POSTGRES_PASSWORD=mlobs",
            "-e", "POSTGRES_DB=mlobs",
            "-p", f"127.0.0.1:{port}:5432",
            "postgres:16-alpine",
        ],
        capture_output=True,
        text=True,
    )
    if run.returncode != 0:
        skip_or_fail(f"docker run postgres:16-alpine failed: {run.stderr.strip()}")

    dsn = f"postgresql://mlobs:mlobs@127.0.0.1:{port}/mlobs"
    try:
        deadline = time.monotonic() + 120
        consecutive_ok = 0
        while consecutive_ok < 2:  # two successes: survive the initdb restart
            try:
                with psycopg.connect(dsn, connect_timeout=2) as conn:
                    conn.execute("SELECT 1")
                consecutive_ok += 1
            except psycopg.OperationalError:
                consecutive_ok = 0
                if time.monotonic() > deadline:
                    skip_or_fail("throwaway postgres did not become ready within 120s")
            time.sleep(0.5)
        with psycopg.connect(dsn) as conn:
            conn.execute(PLAN_S4_DDL)
            conn.commit()
        yield dsn
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)


@pytest.fixture()
def pg_conn(pg_dsn):
    """Fresh connection per test with both tables truncated."""
    import psycopg

    with psycopg.connect(pg_dsn) as conn:
        conn.execute("TRUNCATE predictions, drift_runs RESTART IDENTITY")
        conn.commit()
        yield conn
