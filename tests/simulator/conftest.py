"""Test fixtures for the simulator suite.

Adds the repo root to ``sys.path`` so ``import src.simulator`` resolves when CI
runs the ``pytest`` console script (which, unlike ``python -m pytest``, does not
add the working directory), and provides a fake clock and a mocked HTTP client
so the pacing/posting loop is tested with no live network.
"""

import sys
from pathlib import Path

import httpx
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))


class FakeClock:
    """Monotonic clock that only advances when ``sleep`` is called."""

    def __init__(self) -> None:
        self.now = 0.0
        self.sleeps: list[float] = []

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.sleeps.append(seconds)
        self.now += seconds


class FakeResponse:
    def __init__(self, status_code: int) -> None:
        self.status_code = status_code


class FakeClient:
    """Records POST calls; replays a fixed cycle of behaviors.

    Each behavior is either an int status code or the string ``"fail"`` (raises
    an ``httpx.ConnectError``, mimicking a connection failure).
    """

    def __init__(self, behaviors: list[object]) -> None:
        self._behaviors = list(behaviors)
        self._index = 0
        self.calls: list[tuple[str, dict]] = []

    def post(self, url: str, json: dict) -> FakeResponse:
        self.calls.append((url, json))
        behavior = self._behaviors[self._index % len(self._behaviors)]
        self._index += 1
        if behavior == "fail":
            raise httpx.ConnectError("simulated connection failure")
        return FakeResponse(int(behavior))


@pytest.fixture
def clock() -> FakeClock:
    return FakeClock()


@pytest.fixture
def fake_client_cls() -> type[FakeClient]:
    return FakeClient
