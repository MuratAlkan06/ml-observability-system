"""Model loader and inference wrapper (docs/PLAN.md §2 inference contract).

``torch`` / ``transformers`` are imported lazily inside :func:`load_model` so the
FastAPI app, schemas, metrics, and the whole unit-test suite import cleanly
without the heavy ML wheels installed (tests inject a fake model instead).
"""

from __future__ import annotations

import logging
import time
from typing import Any, Protocol, Tuple

logger = logging.getLogger(__name__)


class Model(Protocol):
    """Minimal interface the API depends on (real model or test fake)."""

    model_version: str

    def predict(self, text: str) -> "Tuple[str, float, int, float]":
        """Return ``(label, confidence, token_count, latency_ms)``."""
        ...


class SentimentModel:
    """DistilBERT SST-2 wrapper around a ``TextClassificationPipeline``."""

    def __init__(self, pipeline: Any, tokenizer: Any, max_length: int, model_version: str) -> None:
        self._pipeline = pipeline
        self._tokenizer = tokenizer
        self._max_length = max_length
        self.model_version = model_version

    def predict(self, text: str) -> Tuple[str, float, int, float]:
        # token_count = len(input_ids) after truncation, INCLUDING [CLS]/[SEP] ⇒ [3, 256].
        # Computed outside the timed window; the pipeline tokenizes identically.
        input_ids = self._tokenizer(
            text, truncation=True, max_length=self._max_length
        )["input_ids"]
        token_count = len(input_ids)

        start = time.perf_counter()
        output = self._pipeline(text, truncation=True, max_length=self._max_length)
        latency_ms = (time.perf_counter() - start) * 1000.0

        # v5 pipelines return list[dict]; unwrap nested lists defensively.
        result: Any = output
        while isinstance(result, list):
            result = result[0]
        label = str(result["label"]).lower()
        confidence = float(result["score"])
        return label, confidence, token_count, latency_ms


def load_model(settings: Any) -> SentimentModel:
    """Load the pinned model/tokenizer and build the inference pipeline.

    Heavy imports live here so importing this module never pulls torch. At runtime
    the container is offline (``HF_HUB_OFFLINE=1``); the snapshot is baked at build.
    """
    import torch
    from transformers import (
        AutoModelForSequenceClassification,
        AutoTokenizer,
        TextClassificationPipeline,
    )

    torch.set_num_threads(settings.num_threads)

    logger.info(
        "loading model %s@%s", settings.model_name, settings.model_revision
    )
    tokenizer = AutoTokenizer.from_pretrained(
        settings.model_name, revision=settings.model_revision
    )
    model = AutoModelForSequenceClassification.from_pretrained(
        settings.model_name,
        revision=settings.model_revision,
        dtype=torch.float32,
    )
    model.eval()
    pipeline = TextClassificationPipeline(
        model=model, tokenizer=tokenizer, device=-1, top_k=1
    )
    logger.info("model loaded (%s)", settings.model_version)
    return SentimentModel(
        pipeline=pipeline,
        tokenizer=tokenizer,
        max_length=settings.max_length,
        model_version=settings.model_version,
    )
