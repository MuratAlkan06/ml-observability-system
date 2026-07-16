"""Posting loop against a mocked HTTP client (docs/PLAN.md §2, §7)."""

from __future__ import annotations

from src.simulator.core import Simulator


def test_posts_correct_url_and_body_shape(clock, fake_client_cls):
    client = fake_client_cls([200, 200])
    sim = Simulator(
        url="http://api:8000/predict",
        corpus=["alpha", "beta"],
        rate_rps=5.0,
        client=client,
        sleep=clock.sleep,
        monotonic=clock.monotonic,
    )
    sim.run(max_requests=3)

    # Correct URL and JSON body shape {"text": ...}; corpus cycles.
    assert client.calls == [
        ("http://api:8000/predict", {"text": "alpha"}),
        ("http://api:8000/predict", {"text": "beta"}),
        ("http://api:8000/predict", {"text": "alpha"}),
    ]


def test_loop_continues_after_failure(clock, fake_client_cls):
    # Middle request raises a connection error; loop must not crash.
    client = fake_client_cls([200, "fail", 200])
    sim = Simulator(
        url="http://api:8000/predict",
        corpus=["one", "two", "three"],
        rate_rps=5.0,
        client=client,
        sleep=clock.sleep,
        monotonic=clock.monotonic,
    )
    stats = sim.run(max_requests=3)

    assert len(client.calls) == 3
    assert stats.sent == 3
    assert stats.ok == 2
    assert stats.err == 1


def test_non_200_counts_as_error(clock, fake_client_cls):
    client = fake_client_cls([500, 422, 200])
    sim = Simulator(
        url="http://api:8000/predict",
        corpus=["a"],
        rate_rps=5.0,
        client=client,
        sleep=clock.sleep,
        monotonic=clock.monotonic,
    )
    stats = sim.run(max_requests=3)

    assert stats.sent == 3
    assert stats.ok == 1
    assert stats.err == 2
