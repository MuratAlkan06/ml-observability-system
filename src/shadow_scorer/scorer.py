"""Shadow scorer read/score/insert/ack loop (v1.1 Slice A).

Mirrors the primary consumer's at-least-once machinery (docs/PLAN.md §3): a
crash between COMMIT and XACK replays the entry, which then conflicts to a no-op
on ``request_id`` — the "ack-after-commit + unique request_id = exactly-once
effect" line, reused here so shadow rows never duplicate.

Per event: parse (§3 field contract) -> re-score ``text`` with the candidate
model -> compute the confidence delta -> batch INSERT ... ON CONFLICT DO NOTHING
in ONE transaction -> XACK only after commit. Malformed entries and per-event
scoring failures are skipped (left unacked) and drained by the recovery/poison
path; the whole batch is never aborted by a single bad entry.
"""

from __future__ import annotations

import logging
import socket
import time
from collections.abc import Sequence

import redis

from . import metrics
from .compare import confidence_delta
from .model import Model
from .parsing import SHADOW_COLUMNS, MalformedEntry, parse_event

logger = logging.getLogger(__name__)

# Frozen constants (v1.1 plan).
STREAM = "mlobs:predictions"
GROUP = "shadow_scorer"

# Loop tuning (v1.1 frozen: COUNT 20 BLOCK 5000; poison after 5 deliveries).
READ_COUNT = 20
BLOCK_MS = 5000
RECOVER_INTERVAL_S = 60
MIN_IDLE_MS = 60000
MAX_DELIVERIES = 5

_INSERT_SQL = (
    f"INSERT INTO shadow_predictions ({', '.join(SHADOW_COLUMNS)}) "
    "VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) "
    "ON CONFLICT (request_id) DO NOTHING"
)


class ShadowScorer:
    """Owns the read/score/insert/ack cycle plus the recovery and poison paths."""

    def __init__(
        self, redis_client, conn, model: Model, consumer_name: str | None = None
    ) -> None:
        self.redis = redis_client
        self.conn = conn  # psycopg connection, autocommit=True; batches use conn.transaction()
        self.model = model
        self.consumer_name = consumer_name or f"{GROUP}-{socket.gethostname()}"
        self._stopped = False

    # -- lifecycle -----------------------------------------------------------

    def ensure_group(self) -> None:
        """Create the consumer group idempotently.

        Start id ``$`` — score from deploy forward, not the ~50k-entry backlog
        (v1.1 D1). MKSTREAM so a cold start (stream not yet created) still works;
        BUSYGROUP is swallowed on restart.
        """
        try:
            self.redis.xgroup_create(STREAM, GROUP, id="$", mkstream=True)
        except redis.ResponseError as exc:
            if "BUSYGROUP" not in str(exc):
                raise

    def stop(self) -> None:
        self._stopped = True

    def run(self) -> None:
        self.ensure_group()
        self.recover()  # drain anything already pending at startup
        last_recover = time.monotonic()
        while not self._stopped:
            self.poll_once()
            self.update_gauges()
            now = time.monotonic()
            if now - last_recover >= RECOVER_INTERVAL_S:
                self.recover()
                last_recover = now

    # -- main path -----------------------------------------------------------

    def poll_once(self) -> int:
        """One XREADGROUP for new entries; process + ack them. Returns count read."""
        resp = self.redis.xreadgroup(
            GROUP,
            self.consumer_name,
            {STREAM: ">"},
            count=READ_COUNT,
            block=BLOCK_MS,
        )
        if not resp:
            return 0
        # resp: [[stream_name, [(entry_id, {field: value}), ...]]]
        entries = resp[0][1]
        self._process(entries)
        return len(entries)

    def _process(self, entries: Sequence) -> None:
        """Score a batch, insert shadow rows in one transaction, then ack them.

        Malformed entries and per-event scoring failures are skipped and left
        unacked (they stay pending for the recovery/poison path). Duplicates
        (ON CONFLICT) are acked because they are already durably stored.
        """
        if not entries:
            return

        valid_ids: list = []
        records: list[tuple] = []
        for entry_id, fields in entries:
            try:
                records.append(self._score(fields))
                valid_ids.append(entry_id)
            except MalformedEntry as exc:
                logger.warning("skipping malformed entry %s: %s", entry_id, exc)
            except Exception as exc:  # scoring failure: skip, never abort the batch
                logger.exception("skipping entry %s (scoring failed): %s", entry_id, exc)

        if not records:
            return

        inserted = self._insert_batch(records)  # commits on success
        self._ack(valid_ids)  # pipelined, only after commit

        metrics.ROWS_INSERTED.inc(inserted)
        metrics.DUPLICATES_SKIPPED.inc(len(records) - inserted)

    def _score(self, fields) -> tuple:
        """Validate + re-score one event; return the shadow_predictions record.

        Observes the comparison/health metrics as a side effect. Raises
        MalformedEntry on a §3 contract violation (routed to the poison path).
        """
        event = parse_event(fields)
        label, confidence, token_count, latency_ms = self.model.predict(event.text)

        delta = confidence_delta(
            event.primary_label, event.primary_confidence, label, confidence
        )

        metrics.EVENTS_SCORED.inc()
        metrics.PREDICTIONS.labels(label=label).inc()
        metrics.COMPARISONS.labels(
            primary_label=event.primary_label, shadow_label=label
        ).inc()
        metrics.CONFIDENCE_RATIO.observe(confidence)
        metrics.CONFIDENCE_DELTA.observe(delta)
        metrics.INFERENCE_DURATION_SECONDS.observe(latency_ms / 1000.0)

        return (
            event.request_id,
            event.ts,
            self.model.model_version,
            label,
            round(confidence, 6),
            token_count,
            round(latency_ms, 2),
            event.primary_label,
            event.primary_confidence,
        )

    def _insert_batch(self, records: Sequence[tuple]) -> int:
        """Batch INSERT in ONE transaction; return rows actually inserted."""
        with self.conn.transaction():
            with self.conn.cursor() as cur:
                cur.executemany(_INSERT_SQL, records)
                return cur.rowcount

    def _ack(self, ids: Sequence) -> None:
        if not ids:
            return
        pipe = self.redis.pipeline(transaction=False)
        for entry_id in ids:
            pipe.xack(STREAM, GROUP, entry_id)
        pipe.execute()

    # -- recovery / poison path ---------------------------------------------

    def recover(self) -> None:
        """Drop poison pills, then reclaim and reprocess stale pending entries."""
        self.drop_poison()
        self.reclaim()

    def drop_poison(self) -> None:
        """Drop entries whose delivery count has exceeded the limit.

        Delivery count > 5 (read from XPENDING ... IDLE 60000 detail) => the
        entry can never be scored/persisted; log ERROR, XACK to drop it, count it.
        """
        pending = self.redis.xpending_range(
            STREAM, GROUP, min="-", max="+", count=READ_COUNT, idle=MIN_IDLE_MS
        )
        poison_ids = [
            p["message_id"] for p in pending if int(p["times_delivered"]) > MAX_DELIVERIES
        ]
        if not poison_ids:
            return
        for entry_id in poison_ids:
            logger.error("dropping poison pill %s (delivery count > %d)", entry_id, MAX_DELIVERIES)
        self.redis.xack(STREAM, GROUP, *poison_ids)
        metrics.EVENTS_DROPPED.inc(len(poison_ids))

    def reclaim(self) -> None:
        """XAUTOCLAIM stale pending entries and route them through _process."""
        cursor = "0-0"
        while True:
            result = self.redis.xautoclaim(
                STREAM,
                GROUP,
                self.consumer_name,
                min_idle_time=MIN_IDLE_MS,
                start_id=cursor,
                count=READ_COUNT,
            )
            # result: [next_cursor, [(entry_id, {field: value}), ...], [deleted_ids]]
            next_cursor, messages = result[0], result[1]
            if messages:
                self._process(messages)
            if not messages or str(next_cursor) == "0-0":
                break
            cursor = next_cursor

    # -- gauges --------------------------------------------------------------

    def update_gauges(self) -> None:
        """Refresh lag (XINFO GROUPS) and pending (XPENDING summary) gauges."""
        try:
            for group in self.redis.xinfo_groups(STREAM):
                if group.get("name") == GROUP:
                    lag = group.get("lag")
                    if lag is not None:
                        metrics.STREAM_LAG.set(lag)
                    break
            summary = self.redis.xpending(STREAM, GROUP)
            metrics.PENDING_ENTRIES.set(summary.get("pending", 0))
        except redis.ResponseError as exc:
            logger.debug("gauge refresh skipped: %s", exc)
