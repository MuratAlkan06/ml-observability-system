"""Build-time candidate-model bake with label-map assertion (v1.1 D2).

Run once during ``docker build`` (network available). Two build-day risks are
retired here so they can never reach runtime:

1. **Label map.** The candidate exposes no meaningful ``id2label``; the frozen
   map is ``LABEL_0 -> negative`` / ``LABEL_1 -> positive``. Two sanity probes —
   one clearly positive, one clearly negative sentence — are pushed through a
   real forward pass. If either maps to the wrong sentiment the build FAILS.

2. **transformers-v5 refusing the Hub's ``.bin`` weights.** The simple path
   (``from_pretrained`` on the pickle) is tried first. If it raises, the weights
   are converted ONCE at build time to a local safetensors dir
   (``torch.load(weights_only=True)`` -> ``load_state_dict`` -> ``save_pretrained``)
   that the runtime loads instead; the runtime never touches pickle.

The runtime image is then offline (``HF_HUB_OFFLINE=1``); this build fails loudly
if the model cannot be prepared, so the container never reaches the network.
"""

from __future__ import annotations

import logging
import os
import sys

from .config import Settings
from .model import load_model, map_label

logger = logging.getLogger(__name__)

# Sanity probes (v1.1 D2). Unambiguous sentiment so a correct map is unmistakable.
POSITIVE_PROBE = "An absolute masterpiece — I loved every single minute of it."
NEGATIVE_PROBE = "A dreadful, boring, painful waste of time that I truly hated."


def _prepare_weights(settings: Settings) -> None:
    """Ensure a loadable model exists locally, converting .bin if v5 refuses it.

    Simple path: ``from_pretrained`` on the Hub ``.bin`` succeeds -> the snapshot
    is cached in HF_HOME and the runtime loads it from cache. Fallback: convert
    the pickle to a local safetensors dir the runtime loads instead.
    """
    import torch
    from transformers import AutoModelForSequenceClassification

    try:
        AutoModelForSequenceClassification.from_pretrained(
            settings.model_name, revision=settings.model_revision, dtype=torch.float32
        )
        logger.info("simple path: Hub .bin weights loaded directly (no conversion)")
        return
    except Exception as exc:  # transformers-v5 pickle refusal -> convert once
        logger.warning("direct .bin load failed (%s); converting to safetensors", exc)

    from huggingface_hub import hf_hub_download
    from transformers import AutoConfig, AutoTokenizer

    config = AutoConfig.from_pretrained(
        settings.model_name, revision=settings.model_revision
    )
    model = AutoModelForSequenceClassification.from_config(config).to(torch.float32)
    bin_path = hf_hub_download(
        settings.model_name, "pytorch_model.bin", revision=settings.model_revision
    )
    state = torch.load(bin_path, weights_only=True, map_location="cpu")
    missing, unexpected = model.load_state_dict(state, strict=False)
    if missing or unexpected:
        # Not necessarily fatal (e.g. tied/renamed keys); the probes are the gate.
        logger.warning(
            "state_dict load: %d missing, %d unexpected keys", len(missing), len(unexpected)
        )

    os.makedirs(settings.local_model_dir, exist_ok=True)
    model.save_pretrained(settings.local_model_dir, safe_serialization=True)
    AutoTokenizer.from_pretrained(
        settings.model_name, revision=settings.model_revision
    ).save_pretrained(settings.local_model_dir)
    logger.info("converted .bin -> safetensors at %s", settings.local_model_dir)


def _assert_label_map(settings: Settings) -> None:
    """Push the two probes through the runtime load path; fail the build on mismatch."""
    # Show the frozen map for build-log auditability.
    logger.info("frozen label map: LABEL_0 -> %s, LABEL_1 -> %s",
                map_label("LABEL_0"), map_label("LABEL_1"))

    model = load_model(settings)
    pos_label, pos_conf, _, _ = model.predict(POSITIVE_PROBE)
    neg_label, neg_conf, _, _ = model.predict(NEGATIVE_PROBE)
    logger.info("probe positive -> %s (%.4f); negative -> %s (%.4f)",
                pos_label, pos_conf, neg_label, neg_conf)

    if pos_label != "positive":
        raise AssertionError(
            f"label-map sanity FAILED: positive probe mapped to {pos_label!r}"
        )
    if neg_label != "negative":
        raise AssertionError(
            f"label-map sanity FAILED: negative probe mapped to {neg_label!r}"
        )


def main() -> int:
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s %(message)s")
    settings = Settings()
    _prepare_weights(settings)
    _assert_label_map(settings)
    logger.info(
        "baked %s@%s; label-map sanity passed", settings.model_name, settings.model_revision
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
