"""Collection guard + fixtures for the shadow scorer suite (v1.1 Slice A).

Mirrors the consumer/drift patterns:
- Pure-logic tests (parsing, confidence-delta math, label map) need only the
  stdlib and run in the bare dev.txt env.
- Tests that import the scorer or the metrics module need ``redis`` /
  ``prometheus_client``; when those are absent we collect_ignore them so bare CI
  stays green (the full suite runs once the light service deps are installed).
- The idempotency test gets a THROWAWAY postgres:16-alpine via ``docker run``
  (never the compose service) with sql/init.sql applied inline. Missing
  prerequisites => SKIP locally; REQUIRE_SHADOW_INTEGRATION=1 makes them fail.
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

REQUIRE_INTEGRATION = os.environ.get("REQUIRE_SHADOW_INTEGRATION") == "1"
INIT_SQL = REPO_ROOT / "sql" / "init.sql"

# Guard test modules whose imports need light service deps not present in the
# bare dev.txt env. scorer.py imports redis and (via metrics) prometheus_client.
_missing: set[str] = set()
for _mod in ("redis", "prometheus_client"):
    try:
        __import__(_mod)
    except ImportError:
        _missing.add(_mod)

collect_ignore_glob: list[str] = []
if _missing:
    collect_ignore_glob += ["test_shadow_poison.py", "test_shadow_idempotency.py"]
if "prometheus_client" in _missing:
    collect_ignore_glob.append("test_shadow_metrics.py")


def skip_or_fail(reason: str) -> None:
    """Skip locally; fail hard when the CI integration job demands execution."""
    if REQUIRE_INTEGRATION:
        pytest.fail(f"REQUIRE_SHADOW_INTEGRATION=1 but {reason}", pytrace=False)
    pytest.skip(reason)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.fixture(scope="session")
def pg_dsn():
    """Throwaway postgres:16-alpine with the full sql/init.sql applied.

    Applying the real init.sql (owned by this slice) doubles as a validation
    that the edited DDL — shadow_predictions + drift_runs.model_version —
    executes cleanly on a fresh volume.
    """
    try:
        import psycopg
    except ImportError:
        skip_or_fail("psycopg is not installed")
    if shutil.which("docker") is None:
        skip_or_fail("docker CLI is not available")

    port = _free_port()
    name = f"mlobs-shadow-it-{uuid.uuid4().hex[:8]}"
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
            conn.execute(INIT_SQL.read_text(encoding="utf-8"))
            conn.commit()
        yield dsn
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)


@pytest.fixture()
def pg_conn(pg_dsn):
    """Autocommit connection per test with shadow_predictions truncated.

    Autocommit matches production (__main__), so the scorer's explicit
    ``conn.transaction()`` block is what commits — the ack-after-commit contract.
    """
    import psycopg

    conn = psycopg.connect(pg_dsn, autocommit=True)
    with conn.cursor() as cur:
        cur.execute("TRUNCATE shadow_predictions RESTART IDENTITY")
    yield conn
    conn.close()
