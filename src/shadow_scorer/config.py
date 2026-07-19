"""Shadow scorer configuration (pydantic-settings; no shared src/common).

Per docs/PLAN.md §7 each service owns its own ``config.py``; this module imports
nothing from other ``src/`` services. Env-driven with prefix ``MLOBS_SHADOW_``.
Defaults are the frozen v1.1 plan values (candidate model pin, tokenizer limits,
metrics port) and Compose service DNS, so the container runs with zero explicit
configuration.
"""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    # ``protected_namespaces=()`` allows ``model_*`` field names without warnings.
    model_config = SettingsConfigDict(
        env_prefix="MLOBS_SHADOW_",
        extra="ignore",
        protected_namespaces=(),
    )

    # --- Infra wiring (Compose DNS; internal-only metrics port) ---
    redis_url: str = "redis://redis:6379/0"
    pg_dsn: str = "postgresql://mlobs:mlobs@postgres:5432/mlobs"
    # Prometheus exposition port (v1.1 frozen: shadow metrics on :9110).
    metrics_port: int = 9110

    # --- Candidate model (v1.1 frozen constants) ---
    model_name: str = "philschmid/MiniLM-L6-H384-uncased-sst2"
    # Frozen commit SHA — reproducible, baked at build, loaded offline at runtime.
    model_revision: str = "0c0ecdc39368f87291727ec084111e89e30b45b2"
    # Public, frozen version string echoed in shadow_predictions rows.
    model_version: str = "minilm-sst2-v1"
    # Shadow tokenizer contract: max_length=256, specials counted, [3, 256].
    max_length: int = 256
    # Shadow torch threads = 1 (frozen).
    num_threads: int = 1
    hf_home: str = "/opt/hf-cache"
    # Local safetensors dir written by the build-time bake ONLY when the Hub's
    # ``.bin`` weights cannot be loaded directly (transformers-v5 fallback path).
    # When present it is the runtime source; otherwise the baked HF cache is.
    local_model_dir: str = "/opt/hf-model"
