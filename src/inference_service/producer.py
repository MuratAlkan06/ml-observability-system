"""Redis Streams producer — the producer half of docs/PLAN.md §3.

Publishes one prediction event per successful ``/predict`` as
``XADD mlobs:predictions MAXLEN ~ 50000 *`` with the frozen flat string field
map. Fire-and-forget: an XADD failure never propagates to the caller; it only
increments ``mlobs_stream_publish_failures_total`` and logs an ERROR.
"""

from __future__ import annotations

import logging
from typing import Any, Mapping

from . import metrics

logger = logging.getLogger(__name__)


class PredictionProducer:
    """Wraps a redis client to publish prediction events and check liveness."""

    def __init__(self, client: Any, stream_name: str, maxlen: int) -> None:
        self._client = client
        self._stream_name = stream_name
        self._maxlen = maxlen

    def publish(self, event: Mapping[str, str]) -> bool:
        """XADD one event. Returns True on success, False on (swallowed) failure.

        Fire-and-forget (§2/§3): failures are counted and logged, never raised —
        prediction availability beats event completeness.
        """
        try:
            self._client.xadd(
                self._stream_name,
                dict(event),
                maxlen=self._maxlen,
                approximate=True,
            )
        except Exception:
            metrics.STREAM_PUBLISH_FAILURES.inc()
            logger.error("XADD to %s failed; event dropped", self._stream_name, exc_info=True)
            return False
        metrics.STREAM_EVENTS_PUBLISHED.inc()
        return True

    def ping(self) -> bool:
        """Return True iff Redis answers PING (within the client's timeout)."""
        try:
            return bool(self._client.ping())
        except Exception:
            return False

    def close(self) -> None:
        """Best-effort close of the underlying client."""
        close = getattr(self._client, "close", None)
        if callable(close):
            try:
                close()
            except Exception:
                logger.debug("error closing redis client", exc_info=True)
