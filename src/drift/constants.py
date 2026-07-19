"""Frozen cross-service constants owned by this slice (docs/PLAN.md §5, Appendix A).

Every value here is frozen by docs/PLAN.md; do not make any of them
configurable. Stdlib-only module.
"""

# Source-table whitelist (v1.1 D5). The model identity (MODEL_VERSION) and the
# SOURCE_TABLE it reads are now runtime config (see config.py) so one image can
# serve both the primary and the shadow drift jobs. Table identifiers cannot be
# SQL-parameterized, so SOURCE_TABLE is validated against this FROZEN set and
# then interpolated into the window query. The set itself is not configurable.
SOURCE_TABLES = ("predictions", "shadow_predictions")

# Window & cadence (PLAN §5): latest-500 count-based window, evaluated every
# 60s; fewer than 200 rows => skip (no drift_runs row).
WINDOW_SIZE = 500
MIN_WINDOW_SAMPLES = 200
EVAL_INTERVAL_SECONDS = 60.0

# Frozen histogram bins (PLAN §5). Bins are [edge_i, edge_{i+1}); the last
# confidence bin is closed: [0.95, 1.00].
TOKEN_LEN_BIN_EDGES = [3, 8, 16, 24, 32, 257]
CONFIDENCE_BIN_EDGES = [0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 1.00]

# Hard-coded critical values (alpha=0.01; PLAN §5). Fire is strict `>`.
CLASS_CHI2_CRITICAL = 6.635  # df=1
LENGTH_CHI2_CRITICAL = 13.277  # df=4
CONFIDENCE_KL_CRITICAL = 0.10  # nats

# Alerting (PLAN §5): per-test-type cooldown, in-memory monotonic timestamps.
ALERT_COOLDOWN_SECONDS = 900.0
SLACK_TIMEOUT_SECONDS = 5.0

# Prometheus exposition port (Appendix A).
METRICS_PORT = 9109

# Metric/alert label values for the three tests (PLAN §5).
TEST_CLASS = "class"
TEST_TOKEN_LENGTH = "token_length"
TEST_CONFIDENCE = "confidence"
ALL_TESTS = (TEST_CLASS, TEST_TOKEN_LENGTH, TEST_CONFIDENCE)

# Baseline artifact invariants (PLAN §5).
BASELINE_SCHEMA_VERSION = 1
BASELINE_SAMPLE_COUNT = 872  # full SST-2 validation split
PROB_SUM_TOLERANCE = 1e-9
