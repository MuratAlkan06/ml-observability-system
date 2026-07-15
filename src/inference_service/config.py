"""Typed configuration for the inference service (pydantic-settings).

Per docs/PLAN.md §7 each service owns its own ``config.py``; there is no shared
``src/common/``. Cross-service constants (stream name, MAXLEN, model version,
tokenizer limits, ports) are frozen by the plan and reproduced here as defaults,
overridable via environment variables for local/compose wiring.
"""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Runtime configuration, populated from the environment.

    Defaults are the frozen plan values (Appendix A / §1 / §2 / §3). ``MODEL_REVISION``
    defaults to the commit SHA resolved at implementation time so the container is
    reproducible and can run fully offline against the baked snapshot.
    """

    # ``protected_namespaces=()`` allows ``model_*`` field names without pydantic warnings.
    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
        protected_namespaces=(),
    )

    # --- Model (§1 model pin, §2 tokenizer contract) ---
    model_name: str = "distilbert/distilbert-base-uncased-finetuned-sst-2-english"
    # Frozen at implementation time: current commit SHA of the HF model repo.
    model_revision: str = "714eb0fa89d2f80546fda750413ed43d93601a13"
    # Public, frozen version string echoed in responses, events, and DB rows.
    model_version: str = "distilbert-sst2-v1"
    max_length: int = 256
    num_threads: int = 2
    hf_home: str = "/opt/hf-cache"

    # --- Redis Streams producer (§3, Appendix A) ---
    redis_url: str = "redis://redis:6379/0"
    stream_name: str = "mlobs:predictions"
    stream_maxlen: int = 50000
    # Health-check PING timeout (§2: 250 ms). Also bounds fire-and-forget XADD.
    redis_timeout_seconds: float = 0.25

    # --- Service ---
    log_level: str = "INFO"
