"""Collection guard + fixtures for the consumer test suite.

Base CI installs ONLY requirements/dev.txt (pytest, httpx, ruff) and runs bare
``pytest`` at the repo root. The consumer package imports ``redis`` and
``psycopg``; when those are absent we must not raise a collection error. If
either import fails we ignore every test module in this directory, so base CI
stays green while the full S2 suite runs locally against live services.

The integration/DB fixtures additionally skip cleanly unless live services are
reachable via MLOBS_TEST_REDIS_URL / MLOBS_TEST_PG_DSN.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

# Make ``import src.consumer`` resolve when running bare ``pytest`` at the root.
_REPO_ROOT = Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

try:
    import psycopg  # noqa: F401
    import redis  # noqa: F401

    _DEPS_AVAILABLE = True
except ImportError:  # bare CI env (only requirements/dev.txt)
    _DEPS_AVAILABLE = False
    # pytest reads this at collection time; skip all test modules in this dir.
    collect_ignore_glob = ["test_*.py"]


if _DEPS_AVAILABLE:
    import pytest

    @pytest.fixture
    def pg_conn():
        """A clean, autocommit psycopg connection to a live test Postgres."""
        dsn = os.environ.get("MLOBS_TEST_PG_DSN")
        if not dsn:
            pytest.skip("MLOBS_TEST_PG_DSN not set; live Postgres required")
        import psycopg

        try:
            conn = psycopg.connect(dsn, autocommit=True)
        except psycopg.OperationalError as exc:
            pytest.skip(f"live Postgres unavailable: {exc}")
        try:
            with conn.cursor() as cur:
                cur.execute("TRUNCATE predictions RESTART IDENTITY")
        except psycopg.errors.UndefinedTable:
            conn.close()
            pytest.skip("predictions table missing; sql/init.sql not applied")
        yield conn
        conn.close()

    @pytest.fixture
    def redis_client():
        """A live test Redis client with a clean predictions stream."""
        url = os.environ.get("MLOBS_TEST_REDIS_URL")
        if not url:
            pytest.skip("MLOBS_TEST_REDIS_URL not set; live Redis required")
        import redis

        client = redis.Redis.from_url(url, decode_responses=True)
        try:
            client.ping()
        except redis.RedisError as exc:
            pytest.skip(f"live Redis unavailable: {exc}")
        client.delete("mlobs:predictions")
        yield client
        client.delete("mlobs:predictions")
        client.close()
