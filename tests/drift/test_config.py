"""DriftSettings tests (v1.1 D5): identity/source-table config + whitelist.

Pure unit tests — no DB, no torch. pydantic-settings is in requirements/dev.txt.
"""

import pytest
from pydantic import ValidationError

from src.drift.config import DriftSettings
from src.drift.constants import SOURCE_TABLES


def test_defaults_reproduce_primary_drift_identity():
    # Default env => the v1.0 primary drift container, byte-identical behavior.
    settings = DriftSettings()
    assert settings.model_version == "distilbert-sst2-v1"
    assert settings.source_table == "predictions"


def test_shadow_env_selects_shadow_identity(monkeypatch):
    monkeypatch.setenv("MODEL_VERSION", "minilm-sst2-v1")
    monkeypatch.setenv("SOURCE_TABLE", "shadow_predictions")
    settings = DriftSettings()
    assert settings.model_version == "minilm-sst2-v1"
    assert settings.source_table == "shadow_predictions"


def test_both_whitelisted_tables_accepted():
    assert set(SOURCE_TABLES) == {"predictions", "shadow_predictions"}
    for table in SOURCE_TABLES:
        assert DriftSettings(source_table=table).source_table == table


def test_source_table_off_whitelist_rejected():
    # Identifiers cannot be SQL-parameterized: a non-whitelisted SOURCE_TABLE
    # must fail fast at settings construction, never reach the query string.
    with pytest.raises(ValidationError, match="SOURCE_TABLE must be one of"):
        DriftSettings(source_table="predictions; DROP TABLE predictions")
    with pytest.raises(ValidationError, match="SOURCE_TABLE must be one of"):
        DriftSettings(source_table="drift_runs")


def test_source_table_env_off_whitelist_rejected(monkeypatch):
    monkeypatch.setenv("SOURCE_TABLE", "not_a_table")
    with pytest.raises(ValidationError, match="SOURCE_TABLE must be one of"):
        DriftSettings()
