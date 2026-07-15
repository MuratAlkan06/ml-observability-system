"""Rate-paced posting loop for the traffic simulator.

The loop and clock/sleep are injectable so the pacing, stats, and failure
handling can be unit-tested with a fake clock and a mocked HTTP client (no live
network). Request failures (connection errors, non-200 responses) are logged
and counted but never crash the loop; Ctrl-C stops it gracefully.
"""

from __future__ import annotations

import itertools
import logging
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass

import httpx

logger = logging.getLogger("mlobs.simulator")


@dataclass
class Stats:
    """Running counters for the posting loop."""

    sent: int = 0
    ok: int = 0
    err: int = 0

    def line(self) -> str:
        return f"sent={self.sent} ok={self.ok} err={self.err}"


class Simulator:
    """Posts corpus texts to the API ``/predict`` endpoint at a target rate."""

    def __init__(
        self,
        *,
        url: str,
        corpus: Sequence[str],
        rate_rps: float,
        client: httpx.Client,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        report_interval: float = 5.0,
    ) -> None:
        if not corpus:
            raise ValueError("corpus must be non-empty")
        if rate_rps <= 0:
            raise ValueError("rate_rps must be positive")
        self.url = url
        self.corpus = list(corpus)
        self.rate_rps = rate_rps
        self.client = client
        self.sleep = sleep
        self.monotonic = monotonic
        self.report_interval = report_interval
        self.stats = Stats()

    def _send_one(self, text: str) -> None:
        self.stats.sent += 1
        try:
            response = self.client.post(self.url, json={"text": text})
        except httpx.HTTPError as exc:
            self.stats.err += 1
            logger.warning("request failed: %s", exc)
            return
        if response.status_code == 200:
            self.stats.ok += 1
        else:
            self.stats.err += 1
            logger.warning("non-200 response: status=%s", response.status_code)

    def run(self, max_requests: int | None = None) -> Stats:
        """Send requests paced to ``rate_rps``.

        Runs until ``max_requests`` are sent, or forever (until Ctrl-C) when it
        is ``None``. Returns the final :class:`Stats`.
        """
        interval = 1.0 / self.rate_rps
        texts = itertools.cycle(self.corpus)
        scheduled = self.monotonic()
        last_report = scheduled
        count = 0
        try:
            while max_requests is None or count < max_requests:
                self._send_one(next(texts))
                count += 1
                now = self.monotonic()
                if now - last_report >= self.report_interval:
                    logger.info("stats %s", self.stats.line())
                    last_report = now
                scheduled += interval
                delay = scheduled - self.monotonic()
                if delay > 0:
                    self.sleep(delay)
        except KeyboardInterrupt:
            logger.info("interrupted; shutting down")
        return self.stats
