"""POST /predict contract tests (docs/PLAN.md §2, producer half of §3)."""

import uuid

RESPONSE_KEYS = {"request_id", "label", "confidence", "model_version", "latency_ms"}
EVENT_KEYS = {
    "request_id",
    "ts_ms",
    "text",
    "token_count",
    "label",
    "confidence",
    "model_version",
    "latency_ms",
}


def test_predict_200_frozen_shape_and_types(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "I absolutely love this movie"})
    assert resp.status_code == 200
    body = resp.json()

    # Exact field set, no extras.
    assert set(body.keys()) == RESPONSE_KEYS

    # request_id is a valid UUID4 string.
    parsed = uuid.UUID(body["request_id"])
    assert parsed.version == 4

    # label frozen enum, lowercased.
    assert body["label"] in {"positive", "negative"}

    # confidence: float, <=6 decimals, within [0.5, 1.0].
    conf = body["confidence"]
    assert isinstance(conf, float)
    assert conf == round(conf, 6)
    assert conf == 0.987654  # round(0.98765432, 6)
    assert 0.5 <= conf <= 1.0

    # latency_ms: float, <=2 decimals, non-negative.
    lat = body["latency_ms"]
    assert isinstance(lat, float)
    assert lat == round(lat, 2)
    assert lat == 12.35  # round(12.34567, 2)
    assert lat >= 0.0

    assert body["model_version"] == "distilbert-sst2-v1"


def test_predict_publishes_event_with_frozen_fields(client_factory):
    with client_factory() as (client, _model, fake_redis):
        resp = client.post("/predict", json={"text": "great film"})
    assert resp.status_code == 200
    body = resp.json()

    assert len(fake_redis.xadd_calls) == 1
    call = fake_redis.xadd_calls[0]
    assert call["name"] == "mlobs:predictions"
    assert call["maxlen"] == 50000
    assert call["approximate"] is True

    fields = call["fields"]
    assert set(fields.keys()) == EVENT_KEYS
    # Flat string map: every value is a str.
    assert all(isinstance(v, str) for v in fields.values())

    assert fields["request_id"] == body["request_id"]
    assert fields["text"] == "great film"
    assert fields["token_count"] == "7"
    assert fields["label"] == "positive"
    assert fields["confidence"] == "0.987654"
    assert fields["latency_ms"] == "12.35"
    assert fields["model_version"] == "distilbert-sst2-v1"
    assert fields["ts_ms"].isdigit()


def test_predict_fire_and_forget_on_publish_failure(client_factory, metric_value):
    before = metric_value("mlobs_stream_publish_failures_total")
    with client_factory(xadd_ok=False) as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "still returns 200"})
    # Prediction still succeeds despite the XADD failure.
    assert resp.status_code == 200
    assert set(resp.json().keys()) == RESPONSE_KEYS
    after = metric_value("mlobs_stream_publish_failures_total")
    assert after == before + 1


def test_predict_503_when_model_not_loaded(client_factory):
    with client_factory(model="missing") as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "hello"})
    assert resp.status_code == 503
    assert resp.json() == {"detail": "model_not_loaded"}


def test_predict_500_on_inference_error_without_trace(client_factory):
    with client_factory(model="raising") as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "hello"})
    assert resp.status_code == 500
    # Body is exactly the frozen shape — no traceback leaked.
    assert resp.json() == {"detail": "internal_error"}


def test_predict_422_missing_text(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={})
    assert resp.status_code == 422
    assert "detail" in resp.json()


def test_predict_422_empty_text(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={"text": ""})
    assert resp.status_code == 422


def test_predict_422_whitespace_only(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "   \t\n "})
    assert resp.status_code == 422


def test_predict_422_too_long(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "a" * 1001})
    assert resp.status_code == 422


def test_predict_422_unknown_field(client_factory):
    with client_factory() as (client, _model, _redis):
        resp = client.post("/predict", json={"text": "hi", "temperature": 0.9})
    assert resp.status_code == 422
