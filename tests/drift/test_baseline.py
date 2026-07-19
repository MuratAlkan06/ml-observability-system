"""Baseline artifact tests (Appendix B S3): committed baseline.json invariants
plus the builder's assertion/smoothing logic as pure functions (no torch)."""

import json
from pathlib import Path

import pytest

from src.drift.baseline import (
    BaselineValidationError,
    laplace_smooth,
    load_baseline,
    raw_probs,
    validate_baseline,
)
from src.drift.constants import (
    CONFIDENCE_BIN_EDGES,
    PROB_SUM_TOLERANCE,
    TOKEN_LEN_BIN_EDGES,
)

REPO_ROOT = Path(__file__).resolve().parents[2]
BASELINE_JSON = REPO_ROOT / "baseline" / "baseline.json"
BASELINE_TSV = REPO_ROOT / "baseline" / "sst2_validation.tsv"

# v1.1 D5: model_version is a validate_baseline parameter (config, not a
# constant). The committed primary artifact declares this identity.
MODEL_VERSION = "distilbert-sst2-v1"


@pytest.fixture(scope="module")
def doc():
    with open(BASELINE_JSON, encoding="utf-8") as fh:
        return json.load(fh)


class TestCommittedBaseline:
    """The committed artifact must satisfy every frozen invariant."""

    def test_validates_against_frozen_schema(self, doc):
        validate_baseline(doc, MODEL_VERSION)  # raises on any violation

    def test_sample_count_is_full_sst2_validation_split(self, doc):
        assert doc["sample_count"] == 872

    def test_edges_match_frozen_values(self, doc):
        assert doc["token_len_bin_edges"] == [3, 8, 16, 24, 32, 257]
        assert doc["confidence_bin_edges"] == [
            0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 1.00,
        ]

    def test_probs_sum_to_one(self, doc):
        assert abs(sum(doc["class_probs"].values()) - 1.0) <= PROB_SUM_TOLERANCE
        assert abs(sum(doc["token_len_probs"]) - 1.0) <= PROB_SUM_TOLERANCE
        assert abs(sum(doc["confidence_probs"]) - 1.0) <= PROB_SUM_TOLERANCE

    def test_smoothed_probs_strictly_positive(self, doc):
        # Laplace add-one guarantees no zero bins baseline-side.
        assert all(p > 0.0 for p in doc["token_len_probs"])
        assert all(p > 0.0 for p in doc["confidence_probs"])

    def test_class_probs_keys_and_identity(self, doc):
        assert set(doc["class_probs"]) == {"negative", "positive"}
        assert doc["schema_version"] == 1
        assert doc["model_version"] == "distilbert-sst2-v1"

    def test_loader_returns_typed_baseline(self):
        baseline = load_baseline(BASELINE_JSON, MODEL_VERSION)
        assert baseline.sample_count == 872
        assert len(baseline.token_len_probs) == 5
        assert len(baseline.confidence_probs) == 10

    def test_committed_tsv_has_872_data_rows(self):
        lines = BASELINE_TSV.read_text(encoding="utf-8").splitlines()
        assert lines[0].split("\t") == ["sentence", "label"]
        assert len(lines) - 1 == 872


class TestLaplaceSmoothing:
    def test_empty_bin_gets_laplace_mass(self):
        # counts [0, 10], K=2, N=10: p = [(0+1)/12, (10+1)/12] = [1/12, 11/12]
        assert laplace_smooth([0, 10]) == pytest.approx([1 / 12, 11 / 12])

    def test_smoothed_probs_sum_to_one(self):
        probs = laplace_smooth([0, 3, 0, 7, 0])
        # K=5, N=10: [(0+1)/15, (3+1)/15, 1/15, 8/15, 1/15] — sums to 15/15
        assert probs == pytest.approx([1 / 15, 4 / 15, 1 / 15, 8 / 15, 1 / 15])
        assert abs(sum(probs) - 1.0) <= PROB_SUM_TOLERANCE
        assert all(p > 0.0 for p in probs)

    def test_raw_probs(self):
        assert raw_probs([49, 51]) == pytest.approx([0.49, 0.51])
        with pytest.raises(BaselineValidationError):
            raw_probs([0, 0])


def valid_doc() -> dict:
    """Minimal document satisfying every frozen invariant."""
    return {
        "schema_version": 1,
        "model_version": "distilbert-sst2-v1",
        "created_at": "2026-07-14T00:00:00+00:00",
        "sample_count": 872,
        "class_probs": {"negative": 0.49, "positive": 0.51},
        "token_len_bin_edges": list(TOKEN_LEN_BIN_EDGES),
        "token_len_probs": [0.2] * 5,
        "confidence_bin_edges": list(CONFIDENCE_BIN_EDGES),
        "confidence_probs": [0.1] * 10,
    }


class TestBuilderAssertionLogic:
    """validate_baseline is the builder's hard-failure gate; prove it bites."""

    def test_valid_document_passes(self):
        validate_baseline(valid_doc(), MODEL_VERSION)

    def test_probs_sum_violation_rejected(self):
        doc = valid_doc()
        doc["token_len_probs"] = [0.2, 0.2, 0.2, 0.2, 0.2000001]  # off by 1e-7 > 1e-9
        with pytest.raises(BaselineValidationError, match="token_len_probs"):
            validate_baseline(doc, MODEL_VERSION)

    def test_class_probs_sum_violation_rejected(self):
        doc = valid_doc()
        doc["class_probs"] = {"negative": 0.49, "positive": 0.52}
        with pytest.raises(BaselineValidationError, match="class_probs"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_token_len_edges_rejected(self):
        doc = valid_doc()
        doc["token_len_bin_edges"] = [3, 8, 16, 24, 32, 256]  # last edge tampered
        with pytest.raises(BaselineValidationError, match="token_len_bin_edges"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_confidence_edges_rejected(self):
        doc = valid_doc()
        doc["confidence_bin_edges"] = doc["confidence_bin_edges"][:-1] + [0.99]
        with pytest.raises(BaselineValidationError, match="confidence_bin_edges"):
            validate_baseline(doc, MODEL_VERSION)

    def test_zero_smoothed_prob_rejected(self):
        # A zero bin in a smoothed list means smoothing was skipped -> would
        # produce zero chi2 expected counts / KL denominators downstream.
        doc = valid_doc()
        doc["confidence_probs"] = [0.0, 0.2] + [0.1] * 8
        with pytest.raises(BaselineValidationError, match="strictly positive"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_schema_version_rejected(self):
        doc = valid_doc()
        doc["schema_version"] = 2
        with pytest.raises(BaselineValidationError, match="schema_version"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_model_version_rejected(self):
        doc = valid_doc()
        doc["model_version"] = "distilbert-sst2-v2"
        with pytest.raises(BaselineValidationError, match="model_version"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_class_keys_rejected(self):
        doc = valid_doc()
        doc["class_probs"] = {"neg": 0.5, "pos": 0.5}
        with pytest.raises(BaselineValidationError, match="class_probs"):
            validate_baseline(doc, MODEL_VERSION)

    def test_wrong_probs_length_rejected(self):
        doc = valid_doc()
        doc["confidence_probs"] = [0.2] * 5
        with pytest.raises(BaselineValidationError, match="confidence_probs"):
            validate_baseline(doc, MODEL_VERSION)

    def test_configured_model_version_accepts_shadow_artifact(self):
        # v1.1 D5: validation compares against the CONFIGURED model_version, so
        # a shadow (minilm) artifact validates when that version is requested and
        # is rejected when the primary version is requested (identity mismatch).
        doc = valid_doc()
        doc["model_version"] = "minilm-sst2-v1"
        validate_baseline(doc, "minilm-sst2-v1")
        with pytest.raises(BaselineValidationError, match="model_version"):
            validate_baseline(doc, "distilbert-sst2-v1")


class TestBuilderPureHelpers:
    """scripts/build_baseline.py pure functions — importable without torch."""

    def test_build_document_from_synthetic_predictions(self):
        from scripts.build_baseline import build_document

        labels = ["negative"] * 3 + ["positive"] * 7
        token_counts = [5] * 10  # all bin 0
        confidences = [0.97] * 10  # all bin 9
        doc = build_document(labels, token_counts, confidences, MODEL_VERSION)

        assert doc["sample_count"] == 10
        # class raw: [3/10, 7/10]
        assert doc["class_probs"]["negative"] == pytest.approx(0.3)
        assert doc["class_probs"]["positive"] == pytest.approx(0.7)
        # token bins [10,0,0,0,0] smoothed: [(10+1)/15, 1/15, 1/15, 1/15, 1/15]
        assert doc["token_len_probs"] == pytest.approx([11 / 15] + [1 / 15] * 4)
        # confidence bins [0]*9+[10] smoothed: [1/20]*9 + [(10+1)/20]
        assert doc["confidence_probs"] == pytest.approx([1 / 20] * 9 + [11 / 20])
        validate_baseline(doc, MODEL_VERSION)  # synthetic doc passes the builder's own gate

    def test_read_sentences_enforces_shape(self, tmp_path):
        from scripts.build_baseline import read_sentences

        assert len(read_sentences(BASELINE_TSV)) == 872

        bad = tmp_path / "bad.tsv"
        bad.write_text("sentence\tlabel\nonly one row\t1\n", encoding="utf-8")
        with pytest.raises(SystemExit, match="expected 872"):
            read_sentences(bad)

        wrong_header = tmp_path / "wrong.tsv"
        wrong_header.write_text("text\ty\nrow\t1\n", encoding="utf-8")
        with pytest.raises(SystemExit, match="unexpected TSV header"):
            read_sentences(wrong_header)

    def test_builder_flag_defaults_reproduce_primary(self):
        # v1.1 D5: with no flags the builder targets the v1.0 primary (so the
        # committed baseline.json reproduces); --model/--revision/--model-version
        # override for the shadow baseline. No torch/model run needed.
        from scripts.build_baseline import (
            DEFAULT_MODEL_ID,
            DEFAULT_MODEL_REVISION,
            DEFAULT_MODEL_VERSION,
            build_parser,
        )

        defaults = build_parser().parse_args([])
        assert defaults.model == DEFAULT_MODEL_ID == "distilbert-base-uncased-finetuned-sst-2-english"
        assert defaults.revision == DEFAULT_MODEL_REVISION == "714eb0fa89d2f80546fda750413ed43d93601a13"
        assert defaults.model_version == DEFAULT_MODEL_VERSION == "distilbert-sst2-v1"
        assert defaults.out.name == "baseline.json"
        assert defaults.tsv.name == "sst2_validation.tsv"

        shadow = build_parser().parse_args(
            [
                "--model", "philschmid/MiniLM-L6-H384-uncased-sst2",
                "--revision", "0c0ecdc39368f87291727ec084111e89e30b45b2",
                "--model-version", "minilm-sst2-v1",
                "--out", "baseline/baseline-minilm.json",
            ]
        )
        assert shadow.model == "philschmid/MiniLM-L6-H384-uncased-sst2"
        assert shadow.revision == "0c0ecdc39368f87291727ec084111e89e30b45b2"
        assert shadow.model_version == "minilm-sst2-v1"
        assert shadow.out.name == "baseline-minilm.json"
