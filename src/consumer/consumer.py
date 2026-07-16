"""At-least-once Redis Streams -> PostgreSQL consumer (docs/PLAN.md §3, §4).

Exactly-once *effect* via ack-after-commit + a UNIQUE request_id:
XREADGROUP -> parse -> one-transaction batch INSERT ... ON CONFLICT DO NOTHING
-> only after COMMIT, pipelined XACK. A crash between commit and ack replays
the entry, which then conflicts to a no-op. Malformed entries are skipped
(never acked in the main path) and drained by the recovery/poison path.
"""

from __future__ import annotations

import logging
import socket
import time
from collections.abc import Sequence

import redis

from . import metrics
from .parsing import COLUMNS, MalformedEntry, parse_entry

logger = logging.getLogger(__name__)

# Frozen constants (docs/PLAN.md §3, Appendix A).
STREAM = "mlobs:predictions"
GROUP = "pg_writer"

# Consumer-loop tuning (frozen by §3).
READ_COUNT = 100
BLOCK_MS = 5000
RECOVER_INTERVAL_S = 60
MIN_IDLE_MS = 60000
MAX_DELIVERIES = 5  # delivery count > 5 -> poison pill

_INSERT_SQL = (
    f"INSERT INTO predictions ({', '.join(COLUMNS)}) "
    "VALUES (%s, %s, %s, %s, %s, %s, %s, %s) "
    "ON CONFLICT (request_id) DO NOTHING"
)


class Consumer:
    """Owns the read/insert/ack cycle plus the recovery and poison paths."""

    def __init__(self, redis_client, conn, consumer_name: str | None = None) -> None:
        self.redis = redis_client
        self.conn = conn  # psycopg connection, autocommit=True; batches use conn.transaction()
        self.consumer_name = consumer_name or f"{GROUP}-{socket.gethostname()}"
        self._stopped = False

    # -- lifecycle -----------------------------------------------------------

    def ensure_group(self) -> None:
        """Create the consumer group idempotently (start id 0, MKSTREAM)."""
        try:
            self.redis.xgroup_create(STREAM, GROUP, id="0", mkstream=True)
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
        """Parse a batch, insert valid rows in one transaction, then ack them.

        Malformed entries are skipped and left unacked (they stay pending for
        the recovery/poison path). Duplicates (ON CONFLICT) are acked because
        they are already durably stored.
        """
        if not entries:
            return
        metrics.EVENTS_CONSUMED.inc(len(entries))

        valid_ids: list = []
        records: list[tuple] = []
        for entry_id, fields in entries:
            try:
                records.append(parse_entry(fields))
                valid_ids.append(entry_id)
            except MalformedEntry as exc:
                logger.warning("skipping malformed entry %s: %s", entry_id, exc)

        if not records:
            return

        with metrics.BATCH_DURATION.time():
            inserted = self._insert_batch(records)  # commits on success
            self._ack(valid_ids)  # pipelined, only after commit

        metrics.ROWS_INSERTED.inc(inserted)
        metrics.DUPLICATES_SKIPPED.inc(len(records) - inserted)

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
        entry can never be persisted; log ERROR, XACK to drop it, and count it.
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
