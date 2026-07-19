-- Migration 002 — shadow second-model comparison (v1.1 Slice A, D3/D5).
--
-- Migration "001" is implicitly the v1 baseline in sql/init.sql, applied once by
-- the postgres entrypoint on a FRESH volume. Fresh volumes already get everything
-- below from init.sql; this idempotent delta is for EXISTING deployments only.
--
-- Apply once on a live box (no Alembic — v1.1 keeps the plain-SQL stance):
--   docker compose exec -T postgres psql -U mlobs -d mlobs < sql/migrations/002_shadow.sql
--
-- Every statement is guarded (IF NOT EXISTS) so re-running is a no-op.

CREATE TABLE IF NOT EXISTS shadow_predictions (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id         UUID             NOT NULL UNIQUE,
    ts                 TIMESTAMPTZ      NOT NULL,
    inserted_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    model_version      TEXT             NOT NULL,
    label              TEXT             NOT NULL CHECK (label IN ('positive', 'negative')),
    confidence         DOUBLE PRECISION NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    token_count        SMALLINT         NOT NULL CHECK (token_count BETWEEN 3 AND 256),
    latency_ms         DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0.0),
    primary_label      TEXT             NOT NULL CHECK (primary_label IN ('positive', 'negative')),
    primary_confidence DOUBLE PRECISION NOT NULL CHECK (primary_confidence >= 0.0 AND primary_confidence <= 1.0)
);
CREATE INDEX IF NOT EXISTS idx_shadow_predictions_ts ON shadow_predictions (ts DESC);

-- Multi-model drift (D5): tag each drift run with the model it evaluated. The
-- NOT NULL DEFAULT backfills existing rows to the primary version.
ALTER TABLE drift_runs
    ADD COLUMN IF NOT EXISTS model_version TEXT NOT NULL DEFAULT 'distilbert-sst2-v1';
