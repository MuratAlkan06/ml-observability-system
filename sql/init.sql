CREATE TABLE predictions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id    UUID             NOT NULL UNIQUE,
    ts            TIMESTAMPTZ      NOT NULL,
    inserted_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    text          TEXT             NOT NULL CHECK (char_length(text) BETWEEN 1 AND 1000),
    token_count   SMALLINT         NOT NULL CHECK (token_count BETWEEN 3 AND 256),
    label         TEXT             NOT NULL CHECK (label IN ('positive', 'negative')),
    confidence    DOUBLE PRECISION NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    model_version TEXT             NOT NULL,
    latency_ms    DOUBLE PRECISION NOT NULL CHECK (latency_ms >= 0.0)
);
CREATE INDEX idx_predictions_ts ON predictions (ts DESC);

CREATE TABLE drift_runs (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    -- v1.1 D5: which model this drift run evaluated. Default keeps the existing
    -- primary drift job byte-identical; the shadow drift job (Slice B) sets it.
    model_version      TEXT             NOT NULL DEFAULT 'distilbert-sst2-v1',
    window_start_ts    TIMESTAMPTZ      NOT NULL,
    window_end_ts      TIMESTAMPTZ      NOT NULL,
    sample_count       INTEGER          NOT NULL CHECK (sample_count > 0),
    class_chi2_stat    DOUBLE PRECISION NOT NULL,
    class_drift        BOOLEAN          NOT NULL,
    length_chi2_stat   DOUBLE PRECISION NOT NULL,
    length_drift       BOOLEAN          NOT NULL,
    confidence_kl_nats DOUBLE PRECISION NOT NULL,
    confidence_drift   BOOLEAN          NOT NULL,
    drift_detected     BOOLEAN          NOT NULL,
    alert_sent         BOOLEAN          NOT NULL DEFAULT FALSE,
    bins               JSONB            NULL
);
CREATE INDEX idx_drift_runs_run_at ON drift_runs (run_at DESC);

-- v1.1 Slice A (D3): shadow second-model comparison. One row per prediction
-- event, re-scored by the candidate model. request_id UNIQUE is the same
-- exactly-once backstop as predictions; primary_label / primary_confidence are
-- denormalized from the event so agreement SQL needs no join (and no race vs
-- the pg_writer commit). Deliberately no FK to predictions.
CREATE TABLE shadow_predictions (
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
CREATE INDEX idx_shadow_predictions_ts ON shadow_predictions (ts DESC);
