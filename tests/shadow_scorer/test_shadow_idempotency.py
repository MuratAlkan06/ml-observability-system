"""Slice A acceptance: re-scoring the same event yields exactly one shadow row.

Uses an ephemeral Postgres so ON CONFLICT (request_id) DO NOTHING is real, not
mocked. The model and the redis ack are faked (idempotency is a DB property).
Proves ack-after-commit: both deliveries are acked because the duplicate is
already durably stored.
"""

from __future__ import annotations

import time
import uuid

from prometheus_client import REGISTRY

from src.shadow_scorer.scorer import ShadowScorer


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


class _FakeModel:
    model_version = "minilm-sst2-v1"

    def predict(self, text: str):
        # Fixed candidate output; disagrees with the primary label above so the
        # comparison/confusion-matrix path is exercised too.
        return ("negative", 0.734512, 7, 15.5)


class _AckPipeline:
    def __init__(self, sink: list) -> None:
        self._sink = sink

    def xack(self, _stream, _group, entry_id) -> None:
        self._sink.append(entry_id)

    def execute(self) -> None:
        pass


class _FakeRedis:
    def __init__(self) -> None:
        self.acked: list = []

    def pipeline(self, transaction: bool = False) -> _AckPipeline:
        return _AckPipeline(self.acked)


def _sample(name: str, labels=None) -> float:
    return REGISTRY.get_sample_value(name, labels or {}) or 0.0


def test_same_event_twice_produces_one_shadow_row(pg_conn):
    rid = str(uuid.uuid4())
    fake_redis = _FakeRedis()
    scorer = ShadowScorer(fake_redis, pg_conn, _FakeModel())

    ins_before = _sample("mlobs_shadow_rows_inserted_total")
    dup_before = _sample("mlobs_shadow_duplicates_skipped_total")
    scored_before = _sample("mlobs_shadow_events_scored_total")

    scorer._process([("1-0", make_event(rid))])
    scorer._process([("2-0", make_event(rid))])  # redelivery

    with pg_conn.cursor() as cur:
        cur.execute(
            "SELECT model_version, label, primary_label, primary_confidence, token_count"
            " FROM shadow_predictions WHERE request_id = %s",
            (rid,),
        )
        rows = cur.fetchall()

    assert len(rows) == 1  # exactly-once effect
    model_version, label, primary_label, primary_confidence, token_count = rows[0]
    assert model_version == "minilm-sst2-v1"
    assert label == "negative"                 # shadow output persisted
    assert primary_label == "positive"         # denormalized primary half
    assert primary_confidence == 0.998712
    assert token_count == 7

    assert _sample("mlobs_shadow_rows_inserted_total") - ins_before == 1.0
    assert _sample("mlobs_shadow_duplicates_skipped_total") - dup_before == 1.0
    assert _sample("mlobs_shadow_events_scored_total") - scored_before == 2.0
    # comparison metric recorded for the primary=positive / shadow=negative cell
    assert _sample(
        "mlobs_shadow_comparisons_total",
        {"primary_label": "positive", "shadow_label": "negative"},
    ) >= 2.0
    # both deliveries acked (duplicate is durably stored -> safe to ack)
    assert fake_redis.acked == ["1-0", "2-0"]
