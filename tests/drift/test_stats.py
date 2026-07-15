"""Hand-computed fixtures for the pure-Python drift math (PLAN §5, Appendix B S3).

Every expected value below is derived by hand in the comments — small integer
counts wherever possible. No scipy/numpy anywhere.
"""

import math

import pytest

from src.drift.constants import (
    CLASS_CHI2_CRITICAL,
    CONFIDENCE_BIN_EDGES,
    CONFIDENCE_KL_CRITICAL,
    LENGTH_CHI2_CRITICAL,
    TOKEN_LEN_BIN_EDGES,
)
from src.drift.stats import (
    bin_index,
    chi2_statistic,
    fires,
    histogram_counts,
    kl_divergence_nats,
)


class TestChi2:
    def test_no_drift_zero_statistic(self):
        # observed [50, 50], expected [50, 50]:
        #   (50-50)^2/50 + (50-50)^2/50 = 0 + 0 = 0  -> far below 6.635
        stat = chi2_statistic([50, 50], [50.0, 50.0])
        assert stat == 0.0
        assert not fires(stat, CLASS_CHI2_CRITICAL)

    def test_firing_case_df1(self):
        # observed [75, 25] vs expected [50, 50]:
        #   (75-50)^2/50 + (25-50)^2/50 = 625/50 + 625/50 = 12.5 + 12.5 = 25.0
        stat = chi2_statistic([75, 25], [50.0, 50.0])
        assert stat == pytest.approx(25.0)
        assert fires(stat, CLASS_CHI2_CRITICAL)  # 25.0 > 6.635

    def test_no_drift_case_df4(self):
        # 5 uniform bins, n=50, expected 10 per bin, observed exactly 10 each:
        #   sum of five 0-terms = 0  -> below 13.277
        stat = chi2_statistic([10, 10, 10, 10, 10], [10.0] * 5)
        assert stat == 0.0
        assert not fires(stat, LENGTH_CHI2_CRITICAL)

    def test_firing_case_df4(self):
        # observed [50, 0, 0, 0, 0] vs expected [10]*5:
        #   (50-10)^2/10 = 1600/10 = 160
        #   four empty bins: (0-10)^2/10 = 10 each -> 40
        #   total = 160 + 40 = 200
        stat = chi2_statistic([50, 0, 0, 0, 0], [10.0] * 5)
        assert stat == pytest.approx(200.0)
        assert fires(stat, LENGTH_CHI2_CRITICAL)  # 200 > 13.277

    def test_small_deviation_does_not_fire(self):
        # observed [55, 45] vs expected [50, 50]:
        #   (5^2)/50 + (5^2)/50 = 0.5 + 0.5 = 1.0  -> 1.0 < 6.635, no fire
        stat = chi2_statistic([55, 45], [50.0, 50.0])
        assert stat == pytest.approx(1.0)
        assert not fires(stat, CLASS_CHI2_CRITICAL)

    def test_empty_expected_cell_conventions(self):
        # Defensive-only branch (unreachable with a smoothed baseline):
        # E=0 with O=0 contributes nothing; E=0 with O>0 is infinite evidence.
        assert chi2_statistic([0, 10], [0.0, 10.0]) == 0.0
        assert chi2_statistic([1, 9], [0.0, 10.0]) == math.inf

    def test_length_mismatch_rejected(self):
        with pytest.raises(ValueError):
            chi2_statistic([1, 2, 3], [1.0, 2.0])


class TestKLDivergence:
    def test_identical_distributions_zero(self):
        # p == q -> every term p_i * ln(1) = 0
        assert kl_divergence_nats([0.5, 0.5], [0.5, 0.5]) == 0.0

    def test_zero_p_convention(self):
        # p = [0, 1], q = [0.5, 0.5]:
        #   bin 0: 0 * ln(0/0.5) = 0 by the frozen convention
        #   bin 1: 1 * ln(1/0.5) = ln 2 = 0.693147...
        stat = kl_divergence_nats([0.0, 1.0], [0.5, 0.5])
        assert stat == pytest.approx(math.log(2.0))
        assert fires(stat, CONFIDENCE_KL_CRITICAL)  # 0.6931 > 0.10

    def test_hand_computed_two_bins(self):
        # p = [0.8, 0.2], q = [0.5, 0.5]:
        #   0.8 * ln(0.8/0.5) + 0.2 * ln(0.2/0.5)
        # = 0.8 * ln(1.6)     + 0.2 * ln(0.4)
        # = 0.8 * 0.470004    + 0.2 * (-0.916291)
        # = 0.376003 - 0.183258 = 0.192745 nats
        stat = kl_divergence_nats([0.8, 0.2], [0.5, 0.5])
        assert stat == pytest.approx(0.8 * math.log(1.6) + 0.2 * math.log(0.4))
        assert stat == pytest.approx(0.192745, abs=1e-6)
        assert fires(stat, CONFIDENCE_KL_CRITICAL)

    def test_small_shift_does_not_fire(self):
        # p = [0.55, 0.45], q = [0.5, 0.5]:
        #   0.55 * ln(1.1) + 0.45 * ln(0.9)
        # = 0.55 * 0.0953102 + 0.45 * (-0.1053605)
        # = 0.0524206 - 0.0474122 = 0.0050084 nats  -> < 0.10, no fire
        stat = kl_divergence_nats([0.55, 0.45], [0.5, 0.5])
        assert stat == pytest.approx(0.0050084, abs=1e-6)
        assert not fires(stat, CONFIDENCE_KL_CRITICAL)

    def test_empty_window_bin_contributes_zero(self):
        # Window mass concentrated in one bin; the empty window bins add
        # exactly nothing, so KL equals the single occupied-bin term:
        #   1.0 * ln(1.0 / 0.1) = ln 10 = 2.302585...
        p = [0.0] * 9 + [1.0]
        q = [0.1] * 10
        assert kl_divergence_nats(p, q) == pytest.approx(math.log(10.0))

    def test_length_mismatch_rejected(self):
        with pytest.raises(ValueError):
            kl_divergence_nats([1.0], [0.5, 0.5])


class TestThresholdBoundary:
    """Fire is STRICT `>`: a statistic exactly at the critical value does NOT fire."""

    @pytest.mark.parametrize(
        "critical", [CLASS_CHI2_CRITICAL, LENGTH_CHI2_CRITICAL, CONFIDENCE_KL_CRITICAL]
    )
    def test_exactly_at_critical_does_not_fire(self, critical):
        assert fires(critical, critical) is False

    @pytest.mark.parametrize(
        "critical", [CLASS_CHI2_CRITICAL, LENGTH_CHI2_CRITICAL, CONFIDENCE_KL_CRITICAL]
    )
    def test_just_above_critical_fires(self, critical):
        assert fires(critical + 1e-9, critical) is True

    def test_frozen_critical_values(self):
        assert CLASS_CHI2_CRITICAL == 6.635
        assert LENGTH_CHI2_CRITICAL == 13.277
        assert CONFIDENCE_KL_CRITICAL == 0.10


class TestBinning:
    def test_token_length_bins(self):
        # Frozen edges [3, 8, 16, 24, 32, 257]; bins [e_i, e_{i+1}).
        assert bin_index(3, TOKEN_LEN_BIN_EDGES) == 0
        assert bin_index(7, TOKEN_LEN_BIN_EDGES) == 0
        assert bin_index(8, TOKEN_LEN_BIN_EDGES) == 1  # boundary goes right
        assert bin_index(15, TOKEN_LEN_BIN_EDGES) == 1
        assert bin_index(16, TOKEN_LEN_BIN_EDGES) == 2
        assert bin_index(24, TOKEN_LEN_BIN_EDGES) == 3
        assert bin_index(31, TOKEN_LEN_BIN_EDGES) == 3
        assert bin_index(32, TOKEN_LEN_BIN_EDGES) == 4
        assert bin_index(256, TOKEN_LEN_BIN_EDGES) == 4  # max legal token_count

    def test_confidence_bins_last_bin_closed(self):
        # Frozen edges 0.50..1.00 step 0.05; last bin [0.95, 1.00] CLOSED.
        assert bin_index(0.50, CONFIDENCE_BIN_EDGES) == 0
        assert bin_index(0.549, CONFIDENCE_BIN_EDGES) == 0
        assert bin_index(0.55, CONFIDENCE_BIN_EDGES) == 1
        assert bin_index(0.9499, CONFIDENCE_BIN_EDGES) == 8
        assert bin_index(0.95, CONFIDENCE_BIN_EDGES) == 9
        assert bin_index(1.0, CONFIDENCE_BIN_EDGES) == 9  # closed right edge

    def test_histogram_counts(self):
        # token counts 5,7 -> bin0; 8 -> bin1; 20 -> bin2; 100,256 -> bin4
        counts = histogram_counts([5, 7, 8, 20, 100, 256], TOKEN_LEN_BIN_EDGES)
        assert counts == [2, 1, 1, 0, 2]
        assert sum(counts) == 6
