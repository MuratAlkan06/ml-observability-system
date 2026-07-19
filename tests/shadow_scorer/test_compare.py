"""Confidence-delta math (v1.1 frozen: d = p_pos(shadow) - p_pos(primary)).

Stdlib-only; runs in the bare dev.txt env. Covers both label directions and the
0.5 edge where p_pos is 0.5 regardless of label.
"""

from __future__ import annotations

import pytest

from src.shadow_scorer.compare import confidence_delta, p_pos


@pytest.mark.parametrize(
    "label, confidence, expected",
    [
        ("positive", 0.90, 0.90),   # winning class is positive -> p_pos == confidence
        ("negative", 0.90, 0.10),   # winning class is negative -> p_pos == 1 - confidence
        ("positive", 0.50, 0.50),   # edge
        ("negative", 0.50, 0.50),   # edge: 1 - 0.5 == 0.5
        ("positive", 1.00, 1.00),
        ("negative", 1.00, 0.00),
    ],
)
def test_p_pos(label, confidence, expected):
    assert p_pos(label, confidence) == pytest.approx(expected)


@pytest.mark.parametrize(
    "p_label, p_conf, s_label, s_conf, expected",
    [
        # shadow less confident-positive than primary -> negative delta
        ("positive", 0.90, "positive", 0.70, -0.20),
        # primary negative (p_pos 0.10) vs shadow positive (p_pos 0.80) -> +0.70
        ("negative", 0.90, "positive", 0.80, 0.70),
        # both negative: primary p_pos 0.10 (conf 0.9), shadow p_pos 0.40 (conf 0.6) -> +0.30
        ("negative", 0.90, "negative", 0.60, 0.30),
        # 0.5 edge on both sides -> exactly 0.0 regardless of labels
        ("positive", 0.50, "negative", 0.50, 0.0),
        ("negative", 0.50, "positive", 0.50, 0.0),
        # full agreement, identical confidence -> 0.0
        ("positive", 0.83, "positive", 0.83, 0.0),
    ],
)
def test_confidence_delta(p_label, p_conf, s_label, s_conf, expected):
    assert confidence_delta(p_label, p_conf, s_label, s_conf) == pytest.approx(expected)


def test_delta_is_shadow_minus_primary_direction():
    # A more positive shadow than primary yields a POSITIVE delta (sign contract).
    d = confidence_delta("positive", 0.60, "positive", 0.95)
    assert d > 0
