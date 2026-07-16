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
