"""GET /metrics inventory (docs/PLAN.md §6, API table)."""

API_METRIC_NAMES = [
    "mlobs_http_requests_total",
    "mlobs_http_request_duration_seconds",
    "mlobs_inference_duration_seconds",
    "mlobs_http_requests_in_flight",
    "mlobs_predictions_total",
    "mlobs_prediction_confidence_ratio",
    "mlobs_model_loaded",
    "mlobs_stream_events_published_total",
    "mlobs_stream_publish_failures_total",
]


def test_all_api_metric_names_present(client_factory):
    with client_factory() as (client, _model, _redis):
        # Exercise both tracked endpoints so labelled series materialise.
        assert client.post("/predict", json={"text": "metrics please"}).status_code == 200
        assert client.get("/health").status_code == 200
        text = client.get("/metrics").text

    for name in API_METRIC_NAMES:
        assert name in text, f"missing metric: {name}"


def test_metrics_endpoint_excluded_and_default_collectors_present(client_factory):
    with client_factory() as (client, _model, _redis):
        client.post("/predict", json={"text": "hello"})
        text = client.get("/metrics").text

    # /metrics must not be counted in the HTTP request metrics.
    assert 'endpoint="/metrics"' not in text
    # Only the two tracked endpoints appear.
    assert 'endpoint="/predict"' in text
    # Default process/platform collectors are enabled on the default registry.
    assert "python_info" in text or "process_" in text
