"""Build-time model bake (docs/PLAN.md §1 / §2).

Run once during ``docker build`` (network available) to download the pinned model
snapshot into ``HF_HOME``. The build fails loudly if this cannot complete, so the
runtime image never needs to reach the network (``HF_HUB_OFFLINE=1``).
"""

from __future__ import annotations

import logging
import sys

from .config import Settings
from .model import load_model


def main() -> int:
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s %(message)s")
    settings = Settings()
    load_model(settings)
    logging.getLogger(__name__).info(
        "baked %s@%s into HF cache", settings.model_name, settings.model_revision
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
