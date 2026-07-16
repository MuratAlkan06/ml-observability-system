"""``python -m src.drift`` — drift detection service entrypoint."""

import logging

from .runner import run_forever

if __name__ == "__main__":
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    run_forever()
