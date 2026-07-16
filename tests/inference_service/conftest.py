"""Test fixtures for the inference service.

Adds ``src/`` to ``sys.path`` (no packaging/config file needed) and injects a fake
model + fake redis client so the suite runs with only light deps — no torch,
transformers, or a live Redis. Matches the CI Tests job, which installs the
light service deps but not the heavy ML wheels.
"""

import sys
from contextlib import contextmanager
from pathlib import Path

SRC = Path(__file__).resolve().parents[2] / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

import pytest  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402
from prometheus_client import REGISTRY  # noqa: E402

from inference_service.app import create_app  # noqa: E402
from inference_service.config import Settings  # noqa: E402

MODEL_VERSION = "distilbert-sst2-v1"


class FakeModel:
    """Deterministic stand-in for SentimentModel."""

    def __init__(self, label="positive", confidence=0.98765432, token_count=7, latency_ms=12.34567):
        self.model_version = MODEL_VERSION
        self._label = label
        self._confidence = confidence
        self._token_count = token_count
        self._latency_ms = latency_ms
        self.calls = []

    def predict(self, text):
        self.calls.append(text)
        return self._label, self._confidence, self._token_count, self._latency_ms


class RaisingModel:
    """Model whose inference always raises — exercises the 500 path."""

    model_version = MODEL_VERSION

    def predict(self, text):
        raise RuntimeError("boom")


class FakeRedis:
    """Records XADD calls; configurable PING/XADD failures."""

    def __init__(self, *, ping_ok=True, xadd_ok=True):
        self._ping_ok = ping_ok
        self._xadd_ok = xadd_ok
        self.xadd_calls = []

    def xadd(self, name, fields, maxlen=None, approximate=True, **kwargs):
        if not self._xadd_ok:
            raise ConnectionError("redis down")
        self.xadd_calls.append(
            {"name": name, "fields": fields, "maxlen": maxlen, "approximate": approximate}
        )
        return b"1-0"

    def ping(self):
        if not self._ping_ok:
            raise ConnectionError("redis down")
        return True

    def close(self):
        pass


@contextmanager
def build_client(*, model="ok", ping_ok=True, xadd_ok=True):
    """Yield ``(client, model_obj, fake_redis)`` with lifespan run via TestClient."""
    settings = Settings()
    fake_redis = FakeRedis(ping_ok=ping_ok, xadd_ok=xadd_ok)

    if model == "ok":
        model_obj = FakeModel()

        def loader(_settings):
            return model_obj
    elif model == "raising":
        model_obj = RaisingModel()

        def loader(_settings):
            return model_obj
    elif model == "missing":
        model_obj = None

        def loader(_settings):
            raise RuntimeError("model load failed")
    else:  # pragma: no cover - guards test misuse
        raise ValueError(f"unknown model mode: {model}")

    app = create_app(settings, model_loader=loader, redis_factory=lambda _s: fake_redis)
    with TestClient(app) as client:
        yield client, model_obj, fake_redis


def counter_value(name, labels=None):
    """Current value of a metric sample, treating missing as 0."""
    return REGISTRY.get_sample_value(name, labels or {}) or 0.0


@pytest.fixture
def client_factory():
    return build_client


@pytest.fixture
def metric_value():
    return counter_value
