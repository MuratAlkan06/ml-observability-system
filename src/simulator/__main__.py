"""CLI entry point: ``python -m src.simulator [options]``.

Drives the inference API with generated traffic. ``--mode drift`` swaps in the
drift corpus designed to trip all three drift tests (docs/PLAN.md §5).
"""

from __future__ import annotations

import argparse
import logging
import sys

import httpx

from .config import DEFAULT_URL, rate_from_env
from .corpus import get_corpus
from .core import Simulator

logger = logging.getLogger("mlobs.simulator")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m src.simulator",
        description="Host-side traffic simulator for the ML observability API.",
    )
    parser.add_argument(
        "--url",
        default=DEFAULT_URL,
        help=f"API predict endpoint (default: {DEFAULT_URL})",
    )
    parser.add_argument(
        "--mode",
        choices=("normal", "drift"),
        default="normal",
        help="corpus: 'normal' baseline traffic or 'drift' (default: normal)",
    )
    parser.add_argument(
        "--rate",
        type=float,
        default=None,
        help="requests/second; overrides the RATE_RPS env var (default: 5)",
    )
    parser.add_argument(
        "--count",
        type=int,
        default=None,
        help="stop after N requests (default: run until Ctrl-C)",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=5.0,
        help="per-request HTTP timeout in seconds (default: 5.0)",
    )
    return parser


def resolve_rate(cli_rate: float | None) -> float:
    """CLI ``--rate`` wins; otherwise fall back to the RATE_RPS env var."""
    if cli_rate is not None:
        if cli_rate <= 0:
            raise SystemExit("--rate must be positive")
        return cli_rate
    return rate_from_env()


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
    )
    rate = resolve_rate(args.rate)
    corpus = get_corpus(args.mode)
    logger.info(
        "starting simulator url=%s mode=%s rate=%.3f rps corpus_size=%d",
        args.url,
        args.mode,
        rate,
        len(corpus),
    )
    with httpx.Client(timeout=args.timeout) as client:
        simulator = Simulator(
            url=args.url,
            corpus=corpus,
            rate_rps=rate,
            client=client,
        )
        stats = simulator.run(max_requests=args.count)
    logger.info("final stats %s", stats.line())
    return 0


if __name__ == "__main__":
    sys.exit(main())
