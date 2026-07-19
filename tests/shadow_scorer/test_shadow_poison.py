"""Poison-pill drop after the 6th delivery (v1.1 reuses the §3 poison path).

Recovery path is unit-tested with a fake redis client (no live services, no ML
wheels). Delivery count > 5 (from XPENDING ... IDLE 60000 detail) is the drop
trigger; entries at the boundary (==5) must survive.
"""

from __future__ import annotations

from prometheus_client import REGISTRY

from src.shadow_scorer.scorer import ShadowScorer


class _FakeRedis:
    """Stub of the redis calls used by ShadowScorer.recover()."""

    def __init__(self, pending: list) -> None:
        self._pending = pending
        self.acked: list = []
        self.xpending_idle = None
        self.xpending_min_idle = None

    def xpending_range(self, _stream, _group, min, max, count, consumername=None, idle=None):
        self.xpending_idle = idle
        return self._pending

    def xack(self, _stream, _group, *ids) -> int:
        self.acked.extend(ids)
        return len(ids)

    def xautoclaim(self, _stream, _group, _consumer, min_idle_time, start_id="0-0", count=None):
        self.xpending_min_idle = min_idle_time
        return ["0-0", [], []]  # nothing left to reclaim


def _sample(name: str) -> float:
    return REGISTRY.get_sample_value(name) or 0.0


def test_poison_pill_dropped_after_sixth_delivery():
    pending = [
        {"message_id": "5-0", "consumer": "c", "time_since_delivered": 61000, "times_delivered": 5},
        {"message_id": "6-0", "consumer": "c", "time_since_delivered": 61000, "times_delivered": 6},
    ]
    fake_redis = _FakeRedis(pending)
    scorer = ShadowScorer(fake_redis, conn=None, model=None, consumer_name="shadow_scorer-test")

    dropped_before = _sample("mlobs_shadow_events_dropped_total")

    scorer.recover()  # drop_poison() + reclaim()

    # only the >5 delivery-count entry is dropped + acked; ==5 survives
    assert fake_redis.acked == ["6-0"]
    assert fake_redis.xpending_idle == 60000  # XPENDING ... IDLE 60000
    assert fake_redis.xpending_min_idle == 60000  # XAUTOCLAIM min-idle 60000
    assert _sample("mlobs_shadow_events_dropped_total") - dropped_before == 1.0
