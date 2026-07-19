"""SlackAlerter unit tests: payload format, cooldown, failure handling.

No real network — the HTTP call is injected. httpx itself is only imported
transitively (it is in requirements/dev.txt).
"""

from datetime import datetime, timezone

from src.drift.alerting import SlackAlerter, format_alert_text
from src.drift.constants import ALERT_COOLDOWN_SECONDS
from src.drift.evaluate import DriftResult

WINDOW_START = datetime(2026, 7, 14, 12, 0, 0, tzinfo=timezone.utc)
WINDOW_END = datetime(2026, 7, 14, 12, 5, 0, tzinfo=timezone.utc)


def make_result(*, class_drift=False, length_drift=False, confidence_drift=False):
    return DriftResult(
        window_start_ts=WINDOW_START,
        window_end_ts=WINDOW_END,
        sample_count=500,
        class_chi2_stat=25.0 if class_drift else 0.5,
        class_drift=class_drift,
        length_chi2_stat=200.0 if length_drift else 1.0,
        length_drift=length_drift,
        confidence_kl_nats=0.9 if confidence_drift else 0.01,
        confidence_drift=confidence_drift,
        drift_detected=class_drift or length_drift or confidence_drift,
        bins={},
    )


class FakeClock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now


class RecordingPost:
    def __init__(self, fail=False):
        self.calls = []
        self.fail = fail

    def __call__(self, url, payload):
        self.calls.append((url, payload))
        if self.fail:
            raise RuntimeError("simulated slack outage")


def make_alerter(post, clock, url="https://hooks.slack.example/T0/B0/x", model_version="distilbert-sst2-v1"):
    return SlackAlerter(url, model_version=model_version, clock=clock, post=post)


def test_payload_text_matches_frozen_format():
    text = format_alert_text("distilbert-sst2-v1", "class", 25.0, 6.635, make_result(class_drift=True))
    assert text == (
        "[mlobs][distilbert-sst2-v1] DRIFT: class stat=25.0000 threshold=6.635 window_n=500"
        " window=[2026-07-14T12:00:00+00:00..2026-07-14T12:05:00+00:00]"
    )


def test_payload_text_carries_shadow_model_version():
    # v1.1 D5: the prefix is [mlobs][<model_version>] so Slack (and the dashboard)
    # can tell primary from shadow drift.
    text = format_alert_text("minilm-sst2-v1", "confidence", 0.9, 0.1, make_result(confidence_drift=True))
    assert text.startswith("[mlobs][minilm-sst2-v1] DRIFT: confidence ")


def test_one_message_per_newly_firing_test():
    post, clock = RecordingPost(), FakeClock()
    alerter = make_alerter(post, clock)
    sent = alerter.send_alerts(
        make_result(class_drift=True, length_drift=True, confidence_drift=True)
    )
    assert sent == ["class", "token_length", "confidence"]
    assert len(post.calls) == 3
    assert all(set(payload) == {"text"} for _, payload in post.calls)
    assert post.calls[1][1]["text"].startswith("[mlobs][distilbert-sst2-v1] DRIFT: token_length stat=200.0000 threshold=13.277")
    assert post.calls[2][1]["text"].startswith("[mlobs][distilbert-sst2-v1] DRIFT: confidence stat=0.9000 threshold=0.1")


def test_non_firing_tests_never_posted():
    post, clock = RecordingPost(), FakeClock()
    alerter = make_alerter(post, clock)
    assert alerter.send_alerts(make_result(length_drift=True)) == ["token_length"]
    assert len(post.calls) == 1


def test_cooldown_suppresses_within_900s_and_rearms_after():
    post, clock = RecordingPost(), FakeClock()
    alerter = make_alerter(post, clock)
    result = make_result(class_drift=True)

    assert alerter.send_alerts(result) == ["class"]
    clock.now += ALERT_COOLDOWN_SECONDS - 0.1  # still inside cooldown
    assert alerter.send_alerts(result) == []
    assert len(post.calls) == 1

    clock.now += 0.1  # exactly 900s elapsed -> no longer inside strict `< cooldown`
    assert alerter.send_alerts(result) == ["class"]
    assert len(post.calls) == 2


def test_cooldown_is_per_test_type():
    post, clock = RecordingPost(), FakeClock()
    alerter = make_alerter(post, clock)
    assert alerter.send_alerts(make_result(class_drift=True)) == ["class"]
    clock.now += 10.0
    # class in cooldown, confidence newly firing -> only confidence posts
    sent = alerter.send_alerts(make_result(class_drift=True, confidence_drift=True))
    assert sent == ["confidence"]


def test_empty_webhook_url_disables_alerting_without_http_attempts():
    post, clock = RecordingPost(), FakeClock()
    alerter = make_alerter(post, clock, url="")
    sent = alerter.send_alerts(
        make_result(class_drift=True, length_drift=True, confidence_drift=True)
    )
    assert sent == []
    assert post.calls == []  # no HTTP attempt at all


def test_delivery_failure_logged_not_raised_and_retried_next_run(caplog):
    post, clock = RecordingPost(fail=True), FakeClock()
    alerter = make_alerter(post, clock)
    result = make_result(class_drift=True)

    with caplog.at_level("ERROR", logger="src.drift.alerting"):
        assert alerter.send_alerts(result) == []  # alert_sent=false upstream
    assert "delivery failed" in caplog.text
    assert len(post.calls) == 1

    # Failure must NOT arm the cooldown: the very next run retries.
    post.fail = False
    clock.now += 60.0
    assert alerter.send_alerts(result) == ["class"]
    assert len(post.calls) == 2


def test_partial_failure_still_reports_successful_tests():
    clock = FakeClock()
    calls = []

    def post(url, payload):
        calls.append(payload["text"])
        if "token_length" in payload["text"]:
            raise RuntimeError("boom")

    alerter = make_alerter(post, clock)
    sent = alerter.send_alerts(
        make_result(class_drift=True, length_drift=True, confidence_drift=True)
    )
    assert sent == ["class", "confidence"]  # >=1 posted -> alert_sent=true upstream
