"""Typed runtime configuration for the drift service (pydantic-settings).

Only deployment-varying values live here. Everything frozen by docs/PLAN.md
(window size, guard, cadence, thresholds, bins, cooldown, port) is a constant
in ``constants.py`` and deliberately NOT configurable.

v1.1 D5: model identity (``model_version``) and the table it reads
(``source_table``) are runtime config so ONE image serves both the primary and
the shadow drift jobs. Defaults reproduce the v1.0 primary drift container
byte-identically.
"""

from __future__ import annotations

from pathlib import Path

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

from .constants import SOURCE_TABLES


class DriftSettings(BaseSettings):
    # protected_namespaces=() silences pydantic's ``model_`` field warning for
    # ``model_version`` (whose env var is MODEL_VERSION, PLAN Appendix A).
    model_config = SettingsConfigDict(
        env_prefix="", extra="ignore", protected_namespaces=()
    )

    # psycopg conninfo URL; must point at the stack's postgres (S2).
    database_url: str = "postgresql://mlobs:mlobs@localhost:5432/mlobs"

    # Committed baseline artifact; mounted read-only in the drift container.
    baseline_path: Path = Path("baseline/baseline.json")

    # Empty string disables alerting; evaluation + drift_runs writes continue
    # (PLAN §0/§5). Never bake this into an image.
    slack_webhook_url: str = ""

    # v1.1 D5: which model this drift job evaluates. Default keeps the primary
    # drift container byte-identical; the shadow job sets MODEL_VERSION.
    model_version: str = "distilbert-sst2-v1"

    # v1.1 D5: which table the window query reads. Whitelisted (identifiers
    # cannot be SQL-parameterized): validated here, interpolated in db.py.
    source_table: str = "predictions"

    @field_validator("source_table")
    @classmethod
    def _source_table_whitelisted(cls, value: str) -> str:
        if value not in SOURCE_TABLES:
            raise ValueError(
                f"SOURCE_TABLE must be one of {SOURCE_TABLES} (got {value!r})"
            )
        return value
