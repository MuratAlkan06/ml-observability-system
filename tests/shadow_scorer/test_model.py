"""Label-map correctness of the shadow wrapper (v1.1 D2 frozen map).

The wrapper's label mapping and token_count clamping are exercised with a fake
pipeline/tokenizer, so this runs without torch/transformers (the heavy imports
in model.py are lazy). The build-time bake certifies the same map with a real
forward pass; this test locks the pure mapping logic.
"""

from __future__ import annotations

import pytest

from src.shadow_scorer.model import ShadowModel, map_label


class _FakeTokenizer:
    def __init__(self, n_ids: int) -> None:
        self._n = n_ids

    def __call__(self, text, truncation, max_length):
        return {"input_ids": list(range(self._n))}


class _FakePipeline:
    def __init__(self, label: str, score: float) -> None:
        self._label = label
        self._score = score

    def __call__(self, text, truncation, max_length):
        # v5 pipelines with top_k=1 return a nested list[list[dict]].
        return [[{"label": self._label, "score": self._score}]]


def _model(label, score, n_ids=7):
    return ShadowModel(
        pipeline=_FakePipeline(label, score),
        tokenizer=_FakeTokenizer(n_ids),
        max_length=256,
        model_version="minilm-sst2-v1",
    )


def test_label_map_label1_is_positive():
    label, conf, tokens, latency = _model("LABEL_1", 0.91).predict("great")
    assert label == "positive"
    assert conf == pytest.approx(0.91)
    assert tokens == 7
    assert latency >= 0.0


def test_label_map_label0_is_negative():
    label, conf, _, _ = _model("LABEL_0", 0.88).predict("awful")
    assert label == "negative"
    assert conf == pytest.approx(0.88)


def test_map_label_passthrough_and_reject():
    assert map_label("LABEL_0") == "negative"
    assert map_label("LABEL_1") == "positive"
    assert map_label("positive") == "positive"     # already-mapped pass-through
    assert map_label("NEGATIVE") == "negative"      # case-insensitive
    with pytest.raises(ValueError):
        map_label("LABEL_2")


def test_token_count_clamped_to_contract():
    # below the floor -> clamped up to 3
    assert _model("LABEL_1", 0.9, n_ids=2).predict("x")[2] == 3
    # above the ceiling -> clamped down to 256
    assert _model("LABEL_1", 0.9, n_ids=300).predict("x")[2] == 256
    # in range -> unchanged
    assert _model("LABEL_1", 0.9, n_ids=42).predict("x")[2] == 42
