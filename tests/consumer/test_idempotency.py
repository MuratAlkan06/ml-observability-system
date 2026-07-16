"""S2 acceptance: replaying the same event yields exactly one row.

Uses a live/ephemeral Postgres so ON CONFLICT (request_id) DO NOTHING is real,
not mocked. Redis is faked only for the ack (idempotency is a DB property).
"""

from __future__ import annotations

import time
import uuid

from prometheus_client import REGISTRY

from src.consumer.consumer import Consumer


def make_event(request_id: str) -> dict:
    return {
        "request_id": request_id,
        "ts_ms": str(int(time.time() * 1000)),
        "text": "a genuinely great movie",
        "token_count": "7",
        "label": "positive",
        "confidence": "0.998712",
        "model_version": "distilbert-sst2-v1",
        "latency_ms": "42.17",
    }


class _AckPipeline:
    def __init__(self, sink: list) -> None:
        self._sink = sink

    def xack(self, _stream, _group, entry_id) -> None:
        self._sink.append(entry_id)

    def execute(self) -> None:
        pass


class _FakeRedis:
    """Just enough to satisfy the pipelined ack in Consumer._process."""

    def __init__(self) -> None:
        self.acked: list = []

    def pipeline(self, transaction: bool = False) -> _AckPipeline:
        return _AckPipeline(self.acked)


def _sample(name: str) -> float:
    return REGISTRY.get_sample_value(name) or 0.0


def test_same_event_twice_produces_one_row(pg_conn):
    request_id = str(uuid.uuid4())
    fake_redis = _FakeRedis()
    consumer = Consumer(fake_redis, pg_conn)

    dup_before = _sample("mlobs_consumer_duplicates_skipped_total")
    ins_before = _sample("mlobs_consumer_rows_inserted_total")

    consumer._process([("1-0", make_event(request_id))])
    consumer._process([("2-0", make_event(request_id))])  # redelivery

    with pg_conn.cursor() as cur:
        cur.execute(
            "SELECT count(*) FROM predictions WHERE request_id = %s", (request_id,)
        )
        assert cur.fetchone()[0] == 1

    assert _sample("mlobs_consumer_rows_inserted_total") - ins_before == 1.0
    assert _sample("mlobs_consumer_duplicates_skipped_total") - dup_before == 1.0
    # both deliveries were acked (duplicate is durably stored -> safe to ack)
    assert fake_redis.acked == ["1-0", "2-0"]
