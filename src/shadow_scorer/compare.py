"""Primary-vs-shadow comparison math (v1.1 frozen: confidence delta).

Pure stdlib so the delta contract is unit-testable without redis, psycopg, or
the ML wheels. ``d = p_pos(shadow) - p_pos(primary)`` where ``p_pos`` is the
probability mass on the positive class, reconstructed from a (label, confidence)
pair whose ``confidence`` is the winning-class max softmax (∈ [0.5, 1.0]).
"""

from __future__ import annotations


def p_pos(label: str, confidence: float) -> float:
    """Positive-class probability from a (label, max-softmax) pair.

    ``confidence`` is the winning class probability, so p(positive) is that value
    when the label is positive and its complement when the label is negative.
    """
    return confidence if label == "positive" else 1.0 - confidence


def confidence_delta(
    primary_label: str,
    primary_confidence: float,
    shadow_label: str,
    shadow_confidence: float,
) -> float:
    """d = p_pos(shadow) - p_pos(primary) (v1.1 frozen direction)."""
    return p_pos(shadow_label, shadow_confidence) - p_pos(primary_label, primary_confidence)
