"""Rate resolution (env/CLI) and rate-paced spacing (docs/PLAN.md §7)."""

from __future__ import annotations

from src.simulator.config import DEFAULT_RATE_RPS, rate_from_env
from src.simulator.core import Simulator
from src.simulator.__main__ import build_parser, resolve_rate


def test_rate_default_is_five_when_unset():
    assert rate_from_env(environ={}) == 5.0
    assert DEFAULT_RATE_RPS == 5.0


def test_rate_env_override():
    assert rate_from_env(environ={"RATE_RPS": "12"}) == 12.0


def test_rate_env_invalid_falls_back_to_default():
    assert rate_from_env(environ={"RATE_RPS": ""}) == 5.0
    assert rate_from_env(environ={"RATE_RPS": "   "}) == 5.0
    assert rate_from_env(environ={"RATE_RPS": "notanumber"}) == 5.0
    assert rate_from_env(environ={"RATE_RPS": "-3"}) == 5.0
    assert rate_from_env(environ={"RATE_RPS": "0"}) == 5.0


def test_cli_rate_overrides_env(monkeypatch):
    monkeypatch.setenv("RATE_RPS", "8")
    # CLI value wins over the env value.
    assert resolve_rate(3.0) == 3.0
    # Falls back to env when no CLI value is given.
    assert resolve_rate(None) == 8.0


def test_resolve_rate_default_when_no_cli_and_no_env(monkeypatch):
    monkeypatch.delenv("RATE_RPS", raising=False)
    assert resolve_rate(None) == 5.0


def test_parser_defaults_and_overrides():
    args = build_parser().parse_args([])
    assert args.mode == "normal"
    assert args.rate is None
    assert args.count is None

    args = build_parser().parse_args(
        ["--mode", "drift", "--rate", "10", "--url", "http://x:8000/predict", "--count", "5"]
    )
    assert args.mode == "drift"
    assert args.rate == 10.0
    assert args.url == "http://x:8000/predict"
    assert args.count == 5


def test_requests_are_spaced_at_target_rate(clock, fake_client_cls):
    rate = 5.0  # -> 1/5 = 0.2s between requests
    n = 6
    sim = Simulator(
        url="http://api/predict",
        corpus=["x"],
        rate_rps=rate,
        client=fake_client_cls([200] * n),
        sleep=clock.sleep,
        monotonic=clock.monotonic,
    )
    stats = sim.run(max_requests=n)

    assert stats.sent == n
    assert len(clock.sleeps) == n
    for delay in clock.sleeps:
        assert abs(delay - 1.0 / rate) < 1e-9


def test_higher_rate_shortens_spacing(clock, fake_client_cls):
    rate = 10.0  # -> 0.1s between requests
    n = 4
    sim = Simulator(
        url="http://api/predict",
        corpus=["x"],
        rate_rps=rate,
        client=fake_client_cls([200] * n),
        sleep=clock.sleep,
        monotonic=clock.monotonic,
    )
    sim.run(max_requests=n)

    assert len(clock.sleeps) == n
    for delay in clock.sleeps:
        assert abs(delay - 0.1) < 1e-9
