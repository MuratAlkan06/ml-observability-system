"""Simulator configuration (stdlib only).

The simulator is a host-side tool, so it deliberately reads its one tunable
(``RATE_RPS``) from the process environment rather than pulling in
pydantic-settings; that convention (docs/PLAN.md §7) is reserved for the
containerized services.
"""

from __future__ import annotations

import os

DEFAULT_URL = "http://localhost:8000/predict"
DEFAULT_RATE_RPS = 5.0
RATE_ENV_VAR = "RATE_RPS"


def rate_from_env(environ: dict[str, str] | None = None) -> float:
    """Resolve the target request rate (requests/second) from the environment.

    Reads ``RATE_RPS`` and falls back to :data:`DEFAULT_RATE_RPS` (5) when the
    variable is unset, blank, non-numeric, or non-positive.
    """
    env = os.environ if environ is None else environ
    raw = env.get(RATE_ENV_VAR)
    if raw is None or not raw.strip():
        return DEFAULT_RATE_RPS
    try:
        value = float(raw)
    except ValueError:
        return DEFAULT_RATE_RPS
    if value <= 0:
        return DEFAULT_RATE_RPS
    return value
