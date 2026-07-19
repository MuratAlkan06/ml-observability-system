"""Candidate-model loader and inference wrapper (v1.1 Slice A).

Consciously duplicates the ~90-line wrapper from
``src/inference_service/model.py`` (docs/PLAN.md §7 no-shared-src rule).
``torch`` / ``transformers`` are imported lazily inside :func:`load_model` so
this module — and the wrapper's label-map logic — import and unit-test cleanly
without the heavy ML wheels installed (tests inject a fake pipeline).

Frozen label map (the candidate exposes no meaningful ``id2label``):
``LABEL_0 -> negative``, ``LABEL_1 -> positive``. The build-time bake runs two
sanity probes through a real forward pass, so a wrong map fails the Docker build.
"""

from __future__ import annotations

import logging
import os
import time
from typing import Any, Protocol, Tuple

logger = logging.getLogger(__name__)

# Frozen label map. Pass-through of already-mapped labels keeps the wrapper
# correct even if a future revision ships a real id2label; the bake probes are
# the authority on the text -> sentiment mapping either way.
LABEL_MAP = {"label_0": "negative", "label_1": "positive"}
_LABELS = frozenset({"positive", "negative"})


def map_label(raw: Any) -> str:
    """Map a raw pipeline label to the frozen {positive, negative} vocabulary."""
    key = str(raw).lower()
    if key in LABEL_MAP:
        return LABEL_MAP[key]
    if key in _LABELS:
        return key
    raise ValueError(f"unexpected model label {raw!r}")


class Model(Protocol):
    """Minimal interface the scorer depends on (real model or test fake)."""

    model_version: str

    def predict(self, text: str) -> "Tuple[str, float, int, float]":
        """Return ``(label, confidence, token_count, latency_ms)``."""
        ...


class ShadowModel:
    """MiniLM SST-2 wrapper around a ``TextClassificationPipeline``."""

    def __init__(self, pipeline: Any, tokenizer: Any, max_length: int, model_version: str) -> None:
        self._pipeline = pipeline
        self._tokenizer = tokenizer
        self._max_length = max_length
        self.model_version = model_version

    def predict(self, text: str) -> Tuple[str, float, int, float]:
        # token_count from the shadow's OWN tokenizer (never the event's), specials
        # counted, then clamped to the frozen [3, 256] contract so the DB CHECK can
        # never be violated by an odd tokenizer edge case.
        input_ids = self._tokenizer(
            text, truncation=True, max_length=self._max_length
        )["input_ids"]
        token_count = max(3, min(256, len(input_ids)))

        start = time.perf_counter()
        output = self._pipeline(text, truncation=True, max_length=self._max_length)
        latency_ms = (time.perf_counter() - start) * 1000.0

        # v5 pipelines return list[dict]; unwrap nested lists defensively.
        result: Any = output
        while isinstance(result, list):
            result = result[0]
        label = map_label(result["label"])
        confidence = float(result["score"])
        return label, confidence, token_count, latency_ms


def _resolve_source(settings: Any) -> Tuple[str, Any]:
    """Pick the load source: converted local safetensors dir if the bake wrote
    one (transformers-v5 ``.bin`` fallback), else the baked model@revision cache.
    """
    local = settings.local_model_dir
    if os.path.isdir(local) and os.path.exists(os.path.join(local, "config.json")):
        logger.info("loading converted safetensors model from %s", local)
        return local, None
    return settings.model_name, settings.model_revision


def load_model(settings: Any) -> ShadowModel:
    """Load the pinned candidate model/tokenizer and build the pipeline.

    Heavy imports live here so importing this module never pulls torch. At runtime
    the container is offline (``HF_HUB_OFFLINE=1``); the snapshot (or its converted
    safetensors form) is produced at build time.
    """
    import torch
    from transformers import (
        AutoModelForSequenceClassification,
        AutoTokenizer,
        TextClassificationPipeline,
    )

    torch.set_num_threads(settings.num_threads)

    source, revision = _resolve_source(settings)
    logger.info("loading model %s@%s", source, revision)
    tokenizer = AutoTokenizer.from_pretrained(source, revision=revision)
    model = AutoModelForSequenceClassification.from_pretrained(
        source, revision=revision, dtype=torch.float32
    )
    model.eval()
    pipeline = TextClassificationPipeline(
        model=model, tokenizer=tokenizer, device=-1, top_k=1
    )
    logger.info("model loaded (%s)", settings.model_version)
    return ShadowModel(
        pipeline=pipeline,
        tokenizer=tokenizer,
        max_length=settings.max_length,
        model_version=settings.model_version,
    )
