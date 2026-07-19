"""One-shot baseline builder (docs/PLAN.md §5).

Runs the pinned SST-2 model over the committed full SST-2 validation split
(baseline/sst2_validation.tsv, 872 sentences) and writes the frozen-schema
baseline/baseline.json. Tokenization/inference is byte-identical to the API
path (PLAN §2): truncation=True, max_length=256, token_count = len(input_ids)
after truncation INCLUDING [CLS]/[SEP], dtype=torch.float32, confidence = max
softmax, label lowercased.

Torch/transformers are needed ONLY to run this script once; they are not
drift-service dependencies. Run from the repo root in a throwaway venv:

    python -m venv /tmp/.venv-baseline && source /tmp/.venv-baseline/bin/activate
    pip install torch==2.13.0 --index-url https://download.pytorch.org/whl/cpu
    pip install transformers==5.13.1 tokenizers==0.22.2
    python scripts/build_baseline.py

PLAN §1 erratum (verified against live PyPI on 2026-07-14): tokenizers 0.23.0
was never published to PyPI (only 0.23.0rc0 / 0.23.1 exist); 0.22.2 is the
highest stable release satisfying transformers 5.13.1's requirement
``tokenizers<=0.23.0,>=0.22.0``. Tokenization output is defined by the
pinned model revision's vocab/tokenizer files, not the tokenizers lib patch
version.

Hard failures (assertions) if any probs list does not sum to 1.0 within 1e-9
or the emitted bin edges differ from the frozen edges.
"""

from __future__ import annotations

import argparse
import csv
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT))

from src.drift.baseline import CLASS_LABELS, laplace_smooth, raw_probs, validate_baseline  # noqa: E402
from src.drift.constants import (  # noqa: E402
    BASELINE_SAMPLE_COUNT,
    BASELINE_SCHEMA_VERSION,
    CONFIDENCE_BIN_EDGES,
    TOKEN_LEN_BIN_EDGES,
)
from src.drift.stats import histogram_counts  # noqa: E402

# Defaults = the v1.0 primary hard-codes so `python scripts/build_baseline.py`
# with no flags reproduces baseline/baseline.json bit-identically (modulo the
# created_at timestamp). v1.1 D5: --model/--revision/--model-version/--out let
# the same script bake the shadow (MiniLM) baseline. The chosen model's OWN
# tokenizer is loaded from its --model id, so token_count uses the right vocab.
DEFAULT_MODEL_ID = "distilbert-base-uncased-finetuned-sst-2-english"
# Pinned HF commit SHA of DEFAULT_MODEL_ID, resolved once on 2026-07-14 via
# https://huggingface.co/api/models/distilbert/distilbert-base-uncased-finetuned-sst-2-english
# (PLAN §1: hard-coded for reproducibility; no floating "main").
DEFAULT_MODEL_REVISION = "714eb0fa89d2f80546fda750413ed43d93601a13"
DEFAULT_MODEL_VERSION = "distilbert-sst2-v1"
MAX_LENGTH = 256


def read_sentences(tsv_path: Path) -> list[str]:
    """Read the committed SST-2 validation TSV (columns: sentence, label)."""
    with open(tsv_path, encoding="utf-8", newline="") as fh:
        reader = csv.DictReader(fh, delimiter="\t")
        if reader.fieldnames != ["sentence", "label"]:
            raise SystemExit(
                f"unexpected TSV header {reader.fieldnames!r}; expected ['sentence', 'label']"
            )
        sentences = [row["sentence"] for row in reader]
    if len(sentences) != BASELINE_SAMPLE_COUNT:
        raise SystemExit(
            f"expected {BASELINE_SAMPLE_COUNT} data rows in {tsv_path}, found {len(sentences)}"
        )
    return sentences


def run_model(
    sentences: list[str], model_id: str, revision: str
) -> tuple[list[str], list[int], list[float]]:
    """Predict every sentence exactly like the API path; return labels,
    token counts (incl. specials, post-truncation) and max-softmax confidences.

    Loads the chosen model's OWN tokenizer from ``model_id``/``revision`` (v1.1
    D5 tokenizer rule: the shadow baseline uses the shadow tokenizer)."""
    import torch
    from transformers import AutoModelForSequenceClassification, AutoTokenizer

    tokenizer = AutoTokenizer.from_pretrained(model_id, revision=revision)
    model = AutoModelForSequenceClassification.from_pretrained(
        model_id, revision=revision, dtype=torch.float32
    )
    model.eval()

    # Frozen label map (PLAN §1 D2): output index 0 -> negative, 1 -> positive
    # (the SST-2 convention). The candidate MiniLM has NO id2label (transformers
    # synthesizes LABEL_0/LABEL_1), so we map by index. A model that DOES declare
    # real sentiment labels (the primary DistilBERT: NEGATIVE/POSITIVE) must
    # agree with this index order or we fail loudly — this preserves the old
    # id2label safety while enabling the label-less shadow model.
    declared = {i: str(label).lower() for i, label in dict(model.config.id2label).items()}
    if set(declared.values()) == set(CLASS_LABELS) and declared != dict(enumerate(CLASS_LABELS)):
        raise SystemExit(
            f"model id2label {declared} conflicts with frozen index map "
            f"{dict(enumerate(CLASS_LABELS))}"
        )

    labels: list[str] = []
    token_counts: list[int] = []
    confidences: list[float] = []
    with torch.no_grad():
        for i, sentence in enumerate(sentences, start=1):
            encoded = tokenizer(
                sentence, truncation=True, max_length=MAX_LENGTH, return_tensors="pt"
            )
            token_counts.append(int(encoded["input_ids"].shape[1]))
            logits = model(**encoded).logits
            probs = torch.softmax(logits, dim=-1)[0]
            confidence = float(probs.max().item())
            idx = int(probs.argmax().item())
            if idx >= len(CLASS_LABELS):
                raise SystemExit(
                    f"model produced {logits.shape[-1]} classes; expected {len(CLASS_LABELS)}"
                )
            labels.append(CLASS_LABELS[idx])
            confidences.append(confidence)
            if i % 100 == 0 or i == len(sentences):
                print(f"  predicted {i}/{len(sentences)}", flush=True)
    return labels, token_counts, confidences


def build_document(
    labels: list[str],
    token_counts: list[int],
    confidences: list[float],
    model_version: str = DEFAULT_MODEL_VERSION,
) -> dict:
    """Assemble the frozen-schema baseline document (PLAN §5).

    class_probs raw; token_len_probs + confidence_probs Laplace add-one
    smoothed (baseline-side-only smoothing). ``model_version`` stamps the
    artifact identity (v1.1 D5).
    """
    class_counts = [labels.count(label) for label in CLASS_LABELS]
    class_probs = raw_probs(class_counts)
    token_len_probs = laplace_smooth(histogram_counts(token_counts, TOKEN_LEN_BIN_EDGES))
    confidence_probs = laplace_smooth(histogram_counts(confidences, CONFIDENCE_BIN_EDGES))
    return {
        "schema_version": BASELINE_SCHEMA_VERSION,
        "model_version": model_version,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "sample_count": len(labels),
        "class_probs": dict(zip(CLASS_LABELS, class_probs)),
        "token_len_bin_edges": list(TOKEN_LEN_BIN_EDGES),
        "token_len_probs": token_len_probs,
        "confidence_bin_edges": list(CONFIDENCE_BIN_EDGES),
        "confidence_probs": confidence_probs,
    }


def build_parser() -> argparse.ArgumentParser:
    """CLI parser. Defaults reproduce the v1.0 primary baseline bit-identically
    (modulo created_at); flags let the same script bake the shadow baseline."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tsv", type=Path, default=REPO_ROOT / "baseline" / "sst2_validation.tsv")
    parser.add_argument("--out", type=Path, default=REPO_ROOT / "baseline" / "baseline.json")
    parser.add_argument("--model", default=DEFAULT_MODEL_ID)
    parser.add_argument("--revision", default=DEFAULT_MODEL_REVISION)
    parser.add_argument("--model-version", default=DEFAULT_MODEL_VERSION)
    return parser


def main() -> None:
    args = build_parser().parse_args()

    sentences = read_sentences(args.tsv)
    print(f"loaded {len(sentences)} sentences from {args.tsv}")
    print(f"running {args.model}@{args.revision} (CPU, float32, max_length={MAX_LENGTH})")
    labels, token_counts, confidences = run_model(sentences, args.model, args.revision)

    doc = build_document(labels, token_counts, confidences, args.model_version)

    # Hard failures (PLAN §5): probs sum to 1 within 1e-9, emitted edges equal
    # the frozen edges, schema invariants hold. validate_baseline raises on
    # any violation; the asserts restate the two named build-time guarantees.
    validate_baseline(doc, args.model_version)
    for name in ("token_len_probs", "confidence_probs"):
        assert abs(sum(doc[name]) - 1.0) <= 1e-9, f"{name} does not sum to 1.0 within 1e-9"
    assert abs(sum(doc["class_probs"].values()) - 1.0) <= 1e-9, "class_probs sum violation"
    assert doc["token_len_bin_edges"] == TOKEN_LEN_BIN_EDGES, "token_len_bin_edges mismatch"
    assert doc["confidence_bin_edges"] == CONFIDENCE_BIN_EDGES, "confidence_bin_edges mismatch"

    args.out.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {args.out}")
    print(f"  sample_count      = {doc['sample_count']}")
    print(f"  class_probs       = {doc['class_probs']}")
    print(f"  token_len_probs   = {doc['token_len_probs']}")
    print(f"  confidence_probs  = {doc['confidence_probs']}")
    print("all baseline assertions passed")


if __name__ == "__main__":
    main()
