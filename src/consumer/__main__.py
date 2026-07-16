"""Consumer entrypoint: start the metrics server, connect, run the loop."""

from __future__ import annotations

import logging
import signal

import psycopg
import redis
from prometheus_client import start_http_server

from .config import Settings
from .consumer import Consumer


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    settings = Settings()

    # Default process/platform collectors stay enabled (default registry).
    start_http_server(settings.metrics_port)

    # socket_timeout=None: XREADGROUP uses BLOCK 5000 (§3); from_url's default 5s
    # socket timeout would otherwise race the block and abort every idle poll.
    redis_client = redis.Redis.from_url(
        settings.redis_url, decode_responses=True, socket_timeout=None
    )
    conn = psycopg.connect(settings.pg_dsn, autocommit=True)
    consumer = Consumer(redis_client, conn)

    def _shutdown(*_args) -> None:
        consumer.stop()

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    try:
        consumer.run()
    finally:
        conn.close()
        redis_client.close()


if __name__ == "__main__":
    main()
