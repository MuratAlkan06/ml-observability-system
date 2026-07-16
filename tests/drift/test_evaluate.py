"""evaluate_window: per-test no-drift and firing cases with hand-computed stats."""

import math
from datetime import datetime, timedelta, timezone

import pytest

from src.drift.baseline import Baseline
from src.drift.constants import CONFIDENCE_BIN_EDGES, TOKEN_LEN_BIN_EDGES
from src.drift.evaluate import WindowRow, evaluate_window

BASE_TS = datetime(2026, 7, 14, 12, 0, 0, tzinfo=timezone.utc)


def uniform_baseline() -> Baseline:
    """Synthetic baseline with uniform bins so expectations are trivial to hand-compute."""
    return Baseline(
        schema_version=1,
        model_version="distilbert-sst2-v1",
        created_at="2026-07-14T00:00:00+00:00",
        sample_count=872,
        class_probs={"negative": 0.5, "positive": 0.5},
        token_len_bin_edges=list(TOKEN_LEN_BIN_EDGES),
        token_len_probs=[0.2] * 5,
        confidence_bin_edges=list(CONFIDENCE_BIN_EDGES),
        confidence_probs=[0.1] * 10,
    )


# One representative value per frozen bin.
TOKEN_PER_BIN = [5, 10, 20, 28, 100]  # bins 0..4
CONF_PER_BIN = [0.52, 0.57, 0.62, 0.67, 0.72, 0.77, 0.82, 0.87, 0.92, 0.97]  # bins 0..9


def make_rows(n, labels, token_counts, confidences):
    return [
        WindowRow(
            label=labels[i % len(labels)],
            token_count=token_counts[i % len(token_counts)],
            confidence=confidences[i % len(confidences)],
            ts=BASE_TS + timedelta(seconds=i),
        )
        for i in range(n)
    ]


def uniform_rows(n=200):
    """Window matching the uniform baseline exactly: all three stats are 0."""
    # n=200: 100 neg / 100 pos, 40 per token bin, 20 per confidence bin.
    return make_rows(
        n,
        labels=["negative", "positive"],
        token_counts=TOKEN_PER_BIN,
        confidences=CONF_PER_BIN,
    )


class TestNoDrift:
    def test_matching_window_produces_zero_stats(self):
        result = evaluate_window(uniform_rows(200), uniform_baseline())
        # class: observed [100, 100] vs expected [100, 100] -> chi2 = 0
        # length: 40 per bin vs 200*0.2 = 40 -> chi2 = 0
        # confidence: p_i = 20/200 = 0.1 vs q_i = 0.1 -> KL = sum 0.1*ln(1) = 0
        assert result.class_chi2_stat == pytest.approx(0.0)
        assert result.length_chi2_stat == pytest.approx(0.0)
        assert result.confidence_kl_nats == pytest.approx(0.0)
        assert not result.class_drift
        assert not result.length_drift
        assert not result.confidence_drift
        assert not result.drift_detected

    def test_window_bounds_and_sample_count(self):
        rows = uniform_rows(200)
        result = evaluate_window(rows, uniform_baseline())
        assert result.sample_count == 200
        assert result.window_start_ts == BASE_TS
        assert result.window_end_ts == BASE_TS + timedelta(seconds=199)


class TestClassDrift:
    def test_all_negative_fires_class_only(self):
        # 200 rows all negative; tokens/confidences stay uniform:
        # class: observed [200, 0] vs expected [100, 100]
        #   (200-100)^2/100 + (0-100)^2/100 = 100 + 100 = 200 > 6.635 -> fire
        rows = make_rows(200, ["negative"], TOKEN_PER_BIN, CONF_PER_BIN)
        result = evaluate_window(rows, uniform_baseline())
        assert result.class_chi2_stat == pytest.approx(200.0)
        assert result.class_drift
        assert not result.length_drift
        assert not result.confidence_drift
        assert result.drift_detected  # any positive => drift_detected


class TestTokenLengthDrift:
    def test_long_text_floods_top_bin(self):
        # 200 rows, every token_count = 100 (bin 4 = [32, 257)):
        # observed [0,0,0,0,200] vs expected [40]*5
        #   4 * (40^2/40) + (200-40)^2/40 = 4*40 + 640 = 800 > 13.277 -> fire
        rows = make_rows(200, ["negative", "positive"], [100], CONF_PER_BIN)
        result = evaluate_window(rows, uniform_baseline())
        assert result.length_chi2_stat == pytest.approx(800.0)
        assert result.length_drift
        assert not result.class_drift
        assert result.drift_detected


class TestConfidenceDrift:
    def test_concentrated_confidence_fires_kl(self):
        # 200 rows, every confidence = 0.97 (bin 9):
        # window p = [0]*9 + [1.0] (RAW, no smoothing) vs q = [0.1]*10
        # KL = 1.0 * ln(1.0/0.1) = ln 10 = 2.302585  (empty bins contribute 0)
        rows = make_rows(200, ["negative", "positive"], TOKEN_PER_BIN, [0.97])
        result = evaluate_window(rows, uniform_baseline())
        assert result.confidence_kl_nats == pytest.approx(math.log(10.0))
        assert result.confidence_drift
        assert not result.class_drift
        assert not result.length_drift
        assert result.drift_detected


class TestBinsDiagnostics:
    def test_bins_payload_shape(self):
        result = evaluate_window(uniform_rows(200), uniform_baseline())
        assert set(result.bins) == {"class", "token_length", "confidence"}
        assert result.bins["class"]["categories"] == ["negative", "positive"]
        assert result.bins["class"]["observed"] == [100, 100]
        assert result.bins["class"]["expected"] == [100.0, 100.0]
        assert result.bins["token_length"]["edges"] == TOKEN_LEN_BIN_EDGES
        assert result.bins["token_length"]["observed"] == [40] * 5
        assert result.bins["confidence"]["edges"] == CONFIDENCE_BIN_EDGES
        assert result.bins["confidence"]["observed"] == [20] * 10
        assert result.bins["confidence"]["expected"] == pytest.approx([20.0] * 10)


def test_empty_window_rejected():
    with pytest.raises(ValueError):
        evaluate_window([], uniform_baseline())
