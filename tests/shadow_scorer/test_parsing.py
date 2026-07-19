"""Event-schema parsing/validation for the shadow scorer (§3 field contract).

Stdlib-only; runs in the bare dev.txt env. Mirrors the primary consumer's
validation so a malformed entry is detected identically here (routed to the
poison path), while only the primary half of the comparison is returned.
"""

from __future__ import annotations

import time
import uuid

import pytest

from src.shadow_scorer.parsing import MalformedEntry, PrimaryEvent, parse_event


def make_fields(**overrides) -> dict:
    fields = {
        "request_id": str(uuid.uuid4()),
        "ts_ms": str(int(time.time() * 1000)),
        "text": "a genuinely great movie",
        "token_count": "7",
        "label": "positive",
        "confidence": "0.998712",
        "model_version": "distilbert-sst2-v1",
        "latency_ms": "42.17",
    }
    fields.update(overrides)
    return fields


def test_valid_event_returns_primary_half():
    rid = str(uuid.uuid4())
    event = parse_event(make_fields(request_id=rid, label="negative", confidence="0.912345"))
    assert isinstance(event, PrimaryEvent)
    assert str(event.request_id) == rid
    assert event.text == "a genuinely great movie"
    assert event.primary_label == "negative"
    assert event.primary_confidence == 0.912345
    # ts is timezone-aware UTC derived from ts_ms.
    assert event.ts.tzinfo is not None


@pytest.mark.parametrize(
    "overrides",
    [
        {"request_id": "not-a-uuid"},
        {"ts_ms": "not-an-int"},
        {"text": ""},                      # below [1, 1000]
        {"text": "x" * 1001},              # above [1, 1000]
        {"token_count": "2"},              # below [3, 256]
        {"token_count": "999"},            # above [3, 256]
        {"token_count": "not-int"},
        {"label": "neutral"},              # not in {positive, negative}
        {"confidence": "1.5"},             # above [0, 1]
        {"confidence": "-0.1"},            # below [0, 1]
        {"confidence": "nan-ish"},
        {"model_version": ""},             # empty
        {"latency_ms": "-1.0"},            # negative
    ],
)
def test_malformed_fields_raise(overrides):
    with pytest.raises(MalformedEntry):
        parse_event(make_fields(**overrides))


def test_missing_field_raises():
    fields = make_fields()
    del fields["confidence"]
    with pytest.raises(MalformedEntry):
        parse_event(fields)


def test_boundary_token_counts_accepted():
    assert parse_event(make_fields(token_count="3")).primary_label == "positive"
    assert parse_event(make_fields(token_count="256")).primary_label == "positive"
