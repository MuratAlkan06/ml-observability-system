"""S2 acceptance: full read/insert/ack cycle against live Redis + Postgres.

XADD synthetic §3 events into real Redis, run one poll cycle, assert rows land
in real Postgres and every valid entry is acked (pending returns to 0). A
second case proves a malformed entry never aborts the batch and stays pending.
"""

from __future__ import annotations

import time
import uuid

from prometheus_client import REGISTRY

from src.consumer.consumer import GROUP, STREAM, Consumer


def make_event(request_id: str) -> dict:
    return {
        "request_id": request_id,
        "ts_ms": str(int(time.time() * 1000)),
        "text": "streamed straight through the pipeline",
        "token_count": "9",
        "label": "negative",
        "confidence": "0.874210",
        "model_version": "distilbert-sst2-v1",
        "latency_ms": "37.44",
    }


def _sample(name: str) -> float:
    return REGISTRY.get_sample_value(name) or 0.0


def test_read_insert_ack_cycle(pg_conn, redis_client):
    consumer = Consumer(redis_client, pg_conn, consumer_name="pg_writer-test")
    consumer.ensure_group()

    ins_before = _sample("mlobs_consumer_rows_inserted_total")
    request_ids = [str(uuid.uuid4()) for _ in range(5)]
    for rid in request_ids:
        redis_client.xadd(STREAM, make_event(rid))

    read = consumer.poll_once()
    assert read == 5

    with pg_conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM predictions")
        assert cur.fetchone()[0] == 5

    assert _sample("mlobs_consumer_rows_inserted_total") - ins_before == 5.0
    # every valid entry acked -> group has no pending entries left
    assert redis_client.xpending(STREAM, GROUP)["pending"] == 0


def test_malformed_entry_does_not_abort_batch(pg_conn, redis_client):
    consumer = Consumer(redis_client, pg_conn, consumer_name="pg_writer-test")
    consumer.ensure_group()

    for _ in range(3):
        redis_client.xadd(STREAM, make_event(str(uuid.uuid4())))
    # token_count out of [3, 256] -> malformed
    bad = make_event(str(uuid.uuid4()))
    bad["token_count"] = "999"
    redis_client.xadd(STREAM, bad)

    read = consumer.poll_once()
    assert read == 4

    with pg_conn.cursor() as cur:
        cur.execute("SELECT count(*) FROM predictions")
        assert cur.fetchone()[0] == 3  # 3 valid inserted, malformed skipped

    # the malformed entry is not acked -> it remains pending for the poison path
    assert redis_client.xpending(STREAM, GROUP)["pending"] == 1
