"""GET /health three-state contract (docs/PLAN.md §2)."""


def test_health_ok(client_factory):
    with client_factory(ping_ok=True) as (client, _model, _redis):
        resp = client.get("/health")
    assert resp.status_code == 200
    assert resp.json() == {
        "status": "ok",
        "model_loaded": True,
        "redis_connected": True,
        "model_version": "distilbert-sst2-v1",
    }


def test_health_degraded_when_redis_down(client_factory):
    with client_factory(ping_ok=False) as (client, _model, _redis):
        resp = client.get("/health")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "degraded"
    assert body["model_loaded"] is True
    assert body["redis_connected"] is False
    assert body["model_version"] == "distilbert-sst2-v1"


def test_health_unavailable_when_model_missing(client_factory):
    with client_factory(model="missing") as (client, _model, _redis):
        resp = client.get("/health")
    assert resp.status_code == 503
    body = resp.json()
    assert body["status"] == "unavailable"
    assert body["model_loaded"] is False
    assert body["model_version"] == "distilbert-sst2-v1"
