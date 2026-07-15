"""Pure-Python drift statistics (docs/PLAN.md §5). Stdlib-only, no scipy/numpy.

Chi-squared and KL divergence are evaluated against hard-coded critical
values (alpha=0.01), which is identical to testing p < 0.01 without carrying
a stats dependency.
"""

from __future__ import annotations

import math
from collections.abc import Sequence


def bin_index(value: float, edges: Sequence[float]) -> int:
    """Map ``value`` to a histogram bin for the frozen edges.

    Bins are ``[edge_i, edge_{i+1})`` with the last bin closed on the right
    (PLAN §5: last confidence bin is ``[0.95, 1.00]``). For token lengths the
    closed right edge (257) is unreachable — token_count <= 256 by contract —
    so the semantics are identical to a half-open last bin.

    Out-of-range values are clamped into the first/last bin. They are
    unreachable through the frozen pipeline (DB CHECK constraints + binary
    max-softmax >= 0.5) and clamping keeps the monitor loop crash-free.
    """
    for i in range(len(edges) - 2):
        if value < edges[i + 1]:
            return i
    return len(edges) - 2


def histogram_counts(values: Sequence[float], edges: Sequence[float]) -> list[int]:
    """Count ``values`` into the ``len(edges) - 1`` frozen bins."""
    counts = [0] * (len(edges) - 1)
    for value in values:
        counts[bin_index(value, edges)] += 1
    return counts


def chi2_statistic(observed: Sequence[int], expected: Sequence[float]) -> float:
    """Pearson chi-squared statistic: sum((O_i - E_i)^2 / E_i).

    Expected counts come from smoothed (or empirically non-zero) baseline
    probabilities, so E_i > 0 in practice. Defensively: an empty expected
    cell contributes 0 when the observed cell is also empty, +inf otherwise.
    """
    if len(observed) != len(expected):
        raise ValueError("observed and expected must have the same length")
    stat = 0.0
    for obs, exp in zip(observed, expected):
        if exp <= 0.0:
            if obs:
                return math.inf
            continue
        diff = obs - exp
        stat += diff * diff / exp
    return stat


def kl_divergence_nats(p: Sequence[float], q: Sequence[float]) -> float:
    """KL(P || Q) = sum(p_i * ln(p_i / q_i)) in nats, convention 0*ln(0/q) = 0.

    Direction is frozen: P is the raw (unsmoothed) production window
    distribution, Q is the Laplace-smoothed baseline, so q_i > 0 always and
    the p_i = 0 convention is the only special case.
    """
    if len(p) != len(q):
        raise ValueError("p and q must have the same length")
    total = 0.0
    for p_i, q_i in zip(p, q):
        if p_i == 0.0:
            continue  # 0 * ln(0 / q) = 0 by convention
        if q_i <= 0.0:
            return math.inf  # unreachable with a smoothed baseline
        total += p_i * math.log(p_i / q_i)
    return total


def fires(stat: float, critical: float) -> bool:
    """Threshold decision, frozen as STRICT ``>`` (a stat exactly at the
    critical value does NOT fire)."""
    return stat > critical
