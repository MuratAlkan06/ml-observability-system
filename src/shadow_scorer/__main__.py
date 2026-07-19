"""Shadow scorer entrypoint: start metrics, load the model, run the loop."""

from __future__ import annotations

import logging
import signal

import psycopg
import redis
from prometheus_client import start_http_server

from .config import Settings
from .model import load_model
from .scorer import ShadowScorer


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    settings = Settings()

    # Default process/platform collectors stay enabled (default registry).
    start_http_server(settings.metrics_port)

    # Load the candidate model before joining the group so the first poll can
    # score immediately (group start id "$" means only post-join events queue).
    model = load_model(settings)

    # socket_timeout=None: XREADGROUP uses BLOCK 5000 (§3); from_url's default 5s
    # socket timeout would otherwise race the block and abort every idle poll.
    redis_client = redis.Redis.from_url(
        settings.redis_url, decode_responses=True, socket_timeout=None
    )
    conn = psycopg.connect(settings.pg_dsn, autocommit=True)
    scorer = ShadowScorer(redis_client, conn, model)

    def _shutdown(*_args) -> None:
        scorer.stop()

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    try:
        scorer.run()
    finally:
        conn.close()
        redis_client.close()


if __name__ == "__main__":
    main()
