"""Typed runtime configuration for the drift service (pydantic-settings).

Only deployment-varying values live here. Everything frozen by docs/PLAN.md
(window size, guard, cadence, thresholds, bins, cooldown, port) is a constant
in ``constants.py`` and deliberately NOT configurable.
"""

from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict


class DriftSettings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="", extra="ignore")

    # psycopg conninfo URL; must point at the stack's postgres (S2).
    database_url: str = "postgresql://mlobs:mlobs@localhost:5432/mlobs"

    # Committed baseline artifact; mounted read-only in the drift container.
    baseline_path: Path = Path("baseline/baseline.json")

    # Empty string disables alerting; evaluation + drift_runs writes continue
    # (PLAN §0/§5). Never bake this into an image.
    slack_webhook_url: str = ""
