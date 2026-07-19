"""Slack incoming-webhook alerting with per-test cooldown (PLAN §5).

Only third-party dep is httpx. Prometheus counters are incremented by the
runner from this module's return value, keeping this module importable in
dependency-light test environments.
"""

from __future__ import annotations

import logging
import time
from collections.abc import Callable

import httpx

from .constants import (
    ALERT_COOLDOWN_SECONDS,
    CLASS_CHI2_CRITICAL,
    CONFIDENCE_KL_CRITICAL,
    LENGTH_CHI2_CRITICAL,
    SLACK_TIMEOUT_SECONDS,
    TEST_CLASS,
    TEST_CONFIDENCE,
    TEST_TOKEN_LENGTH,
)
from .evaluate import DriftResult

logger = logging.getLogger(__name__)


def _default_post(url: str, payload: dict) -> None:
    """POST the Slack payload; raise on transport errors or non-2xx."""
    response = httpx.post(url, json=payload, timeout=SLACK_TIMEOUT_SECONDS)
    response.raise_for_status()


def format_alert_text(
    model_version: str, test: str, stat: float, threshold: float, result: DriftResult
) -> str:
    """Frozen payload text (PLAN §5, v1.1 D5 model_version prefix):
    ``[mlobs][<model_version>] DRIFT: <test> stat=<v> threshold=<v> window_n=<n> window=[<start>..<end>]``
    """
    return (
        f"[mlobs][{model_version}] DRIFT: {test} stat={stat:.4f} threshold={threshold:g}"
        f" window_n={result.sample_count}"
        f" window=[{result.window_start_ts.isoformat()}..{result.window_end_ts.isoformat()}]"
    )


class SlackAlerter:
    """Posts one message per NEWLY-firing test, 900s cooldown per test type.

    - Empty webhook URL disables alerting entirely (no HTTP attempt);
      evaluation and drift_runs writes continue upstream.
    - Cooldown timestamps are in-memory monotonic; a restart may re-alert
      once (accepted, documented in PLAN §5).
    - Delivery failure: log ERROR, message counts as not sent (so
      alert_sent=false if nothing else posted), cooldown NOT armed (retry on
      the next evaluation), never crash the loop.
    """

    def __init__(
        self,
        webhook_url: str,
        *,
        model_version: str,
        cooldown_seconds: float = ALERT_COOLDOWN_SECONDS,
        clock: Callable[[], float] = time.monotonic,
        post: Callable[[str, dict], None] = _default_post,
    ) -> None:
        self._webhook_url = webhook_url
        self._model_version = model_version
        self._cooldown_seconds = cooldown_seconds
        self._clock = clock
        self._post = post
        self._last_sent: dict[str, float] = {}

    def send_alerts(self, result: DriftResult) -> list[str]:
        """Alert on every firing test not in cooldown; return tests actually posted."""
        if not self._webhook_url:
            return []

        firing = [
            (TEST_CLASS, result.class_drift, result.class_chi2_stat, CLASS_CHI2_CRITICAL),
            (TEST_TOKEN_LENGTH, result.length_drift, result.length_chi2_stat, LENGTH_CHI2_CRITICAL),
            (TEST_CONFIDENCE, result.confidence_drift, result.confidence_kl_nats, CONFIDENCE_KL_CRITICAL),
        ]
        sent: list[str] = []
        for test, drifted, stat, threshold in firing:
            if not drifted:
                continue
            now = self._clock()
            last = self._last_sent.get(test)
            if last is not None and now - last < self._cooldown_seconds:
                logger.debug("alert for %s suppressed by cooldown", test)
                continue
            text = format_alert_text(self._model_version, test, stat, threshold, result)
            try:
                self._post(self._webhook_url, {"text": text})
            except Exception as exc:
                # Deliberately no traceback/exception message: httpx errors
                # embed the request URL, which is the secret webhook itself.
                logger.error(
                    "slack alert delivery failed for test=%s (%s)", test, type(exc).__name__
                )
                continue
            self._last_sent[test] = now
            sent.append(test)
            logger.info("slack alert posted for test=%s", test)
        return sent
