"""Frozen baseline artifact: schema validation, smoothing, loading (PLAN §5).

Stdlib-only so `scripts/build_baseline.py` and the pure unit tests can reuse
the exact validation/smoothing logic without torch or any service deps.
"""

from __future__ import annotations

import json
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

from .constants import (
    BASELINE_SCHEMA_VERSION,
    CONFIDENCE_BIN_EDGES,
    PROB_SUM_TOLERANCE,
    TOKEN_LEN_BIN_EDGES,
)

CLASS_LABELS = ("negative", "positive")


class BaselineValidationError(ValueError):
    """The baseline document violates a frozen invariant."""


@dataclass(frozen=True)
class Baseline:
    schema_version: int
    model_version: str
    created_at: str
    sample_count: int
    class_probs: dict[str, float]  # raw (unsmoothed), keys negative/positive
    token_len_bin_edges: list[int]
    token_len_probs: list[float]  # Laplace add-one smoothed, 5 values
    confidence_bin_edges: list[float]
    confidence_probs: list[float]  # Laplace add-one smoothed, 10 values


def laplace_smooth(counts: Sequence[int]) -> list[float]:
    """Add-one Laplace smoothing: p_i = (count_i + 1) / (N + K).

    Build-time, BASELINE-SIDE ONLY (PLAN §5) — guarantees every baseline bin
    is > 0 so chi-squared expected counts and KL denominators are never zero.
    """
    n = sum(counts)
    k = len(counts)
    return [(count + 1) / (n + k) for count in counts]


def raw_probs(counts: Sequence[int]) -> list[float]:
    """Unsmoothed empirical probabilities count_i / N."""
    n = sum(counts)
    if n <= 0:
        raise BaselineValidationError("cannot derive probabilities from zero counts")
    return [count / n for count in counts]


def _check_probs(name: str, probs: object, length: int, *, positive: bool) -> None:
    if not isinstance(probs, list) or len(probs) != length:
        raise BaselineValidationError(f"{name} must be a list of {length} values")
    if not all(isinstance(p, (int, float)) and not isinstance(p, bool) for p in probs):
        raise BaselineValidationError(f"{name} must contain only numbers")
    if any(p < 0.0 or p > 1.0 for p in probs):
        raise BaselineValidationError(f"{name} values must lie in [0, 1]")
    if positive and any(p <= 0.0 for p in probs):
        raise BaselineValidationError(f"{name} must be strictly positive (Laplace-smoothed)")
    total = sum(probs)
    if abs(total - 1.0) > PROB_SUM_TOLERANCE:
        raise BaselineValidationError(
            f"{name} must sum to 1.0 within {PROB_SUM_TOLERANCE} (got {total!r})"
        )


def validate_baseline(doc: dict, model_version: str) -> None:
    """Hard-fail if ``doc`` violates the frozen baseline schema (PLAN §5).

    Enforces: schema_version 1, the CONFIGURED ``model_version`` (v1.1 D5: a
    parameter, not an imported constant, so the shadow baseline validates
    against ``minilm-sst2-v1``), probs lists summing to 1.0 within 1e-9,
    smoothed lists strictly positive, and bin edges exactly equal to the frozen
    edges.
    """
    if doc.get("schema_version") != BASELINE_SCHEMA_VERSION:
        raise BaselineValidationError(
            f"schema_version must be {BASELINE_SCHEMA_VERSION} (got {doc.get('schema_version')!r})"
        )
    if doc.get("model_version") != model_version:
        raise BaselineValidationError(
            f"model_version must be {model_version!r} (got {doc.get('model_version')!r})"
        )
    if not isinstance(doc.get("created_at"), str) or not doc["created_at"]:
        raise BaselineValidationError("created_at must be a non-empty ISO-8601 string")
    sample_count = doc.get("sample_count")
    if not isinstance(sample_count, int) or sample_count <= 0:
        raise BaselineValidationError("sample_count must be a positive integer")

    class_probs = doc.get("class_probs")
    if not isinstance(class_probs, dict) or set(class_probs) != set(CLASS_LABELS):
        raise BaselineValidationError("class_probs must have exactly keys negative/positive")
    _check_probs("class_probs", [class_probs[label] for label in CLASS_LABELS], 2, positive=False)

    if doc.get("token_len_bin_edges") != TOKEN_LEN_BIN_EDGES:
        raise BaselineValidationError(
            f"token_len_bin_edges must equal frozen edges {TOKEN_LEN_BIN_EDGES}"
        )
    if doc.get("confidence_bin_edges") != CONFIDENCE_BIN_EDGES:
        raise BaselineValidationError(
            f"confidence_bin_edges must equal frozen edges {CONFIDENCE_BIN_EDGES}"
        )
    _check_probs("token_len_probs", doc.get("token_len_probs"), len(TOKEN_LEN_BIN_EDGES) - 1, positive=True)
    _check_probs(
        "confidence_probs", doc.get("confidence_probs"), len(CONFIDENCE_BIN_EDGES) - 1, positive=True
    )


def load_baseline(path: Path | str, model_version: str) -> Baseline:
    """Load and validate a committed baseline.json (read-only mount in Docker).

    ``model_version`` is the configured identity the artifact must declare
    (v1.1 D5), so the shadow job loads ``baseline-minilm.json``.
    """
    with open(path, encoding="utf-8") as fh:
        doc = json.load(fh)
    validate_baseline(doc, model_version)
    return Baseline(
        schema_version=doc["schema_version"],
        model_version=doc["model_version"],
        created_at=doc["created_at"],
        sample_count=doc["sample_count"],
        class_probs={label: float(doc["class_probs"][label]) for label in CLASS_LABELS},
        token_len_bin_edges=list(doc["token_len_bin_edges"]),
        token_len_probs=[float(p) for p in doc["token_len_probs"]],
        confidence_bin_edges=[float(e) for e in doc["confidence_bin_edges"]],
        confidence_probs=[float(p) for p in doc["confidence_probs"]],
    )
