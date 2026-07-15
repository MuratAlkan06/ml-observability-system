"""Consumer configuration (pydantic-settings; docs/PLAN.md §7 — no shared src/common).

Env-driven, prefix ``MLOBS_``. Defaults match Compose service DNS and the
Appendix A port table so the container runs with zero explicit configuration.
"""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="MLOBS_", extra="ignore")

    # redis://<host>:<port>/<db> — Compose DNS name ``redis`` on 6379 (Appendix A).
    redis_url: str = "redis://redis:6379/0"
    # libpq DSN — Compose DNS name ``postgres`` on 5432 (Appendix A).
    pg_dsn: str = "postgresql://mlobs:mlobs@postgres:5432/mlobs"
    # Prometheus exposition port (Appendix A: consumer-metrics 9108).
    metrics_port: int = 9108
