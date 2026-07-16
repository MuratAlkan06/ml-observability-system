"""Corpus selection and drift-corpus structural properties (docs/PLAN.md §5)."""

from __future__ import annotations

import pytest

from src.simulator.corpus import DRIFT_CORPUS, NORMAL_CORPUS, get_corpus


def _word_count(text: str) -> int:
    return len(text.split())


def test_mode_normal_returns_normal_corpus():
    assert get_corpus("normal") is NORMAL_CORPUS


def test_mode_drift_returns_drift_corpus():
    assert get_corpus("drift") is DRIFT_CORPUS


def test_unknown_mode_raises():
    with pytest.raises(ValueError):
        get_corpus("bogus")


@pytest.mark.parametrize("corpus", [NORMAL_CORPUS, DRIFT_CORPUS])
def test_all_texts_satisfy_api_input_contract(corpus):
    # docs/PLAN.md §2: <=1000 chars and >=1 non-whitespace char.
    assert corpus, "corpus must be non-empty"
    for text in corpus:
        assert 0 < len(text) <= 1000
        assert text.strip() != ""


def test_normal_corpus_spans_length_range_and_is_balanced():
    # Corrected invariant (S5 E2E finding): a corpus of only SHORT texts fires
    # the token-length drift test even on "normal" traffic, because the frozen
    # baseline spreads ~53% of its mass into the two longest token bins. The
    # normal corpus must therefore SPAN the length range (a few very short,
    # most medium, many long) so the production window tracks the baseline's
    # token_len_probs and no test fires.
    word_counts = [_word_count(t) for t in NORMAL_CORPUS]
    assert min(word_counts) <= 6, "need very short texts for the low token bins"
    assert max(word_counts) >= 30, "need long texts for the fat [32, 257) bin"
    # A genuine spread across tiers, not a single length band.
    assert any(w < 12 for w in word_counts)
    assert any(12 <= w < 22 for w in word_counts)
    assert any(w >= 22 for w in word_counts)
    # Roughly class-balanced -> even corpus size.
    assert len(NORMAL_CORPUS) % 2 == 0


def test_drift_corpus_texts_are_long():
    # >=80% of drift texts must have >=30 whitespace-delimited words so they
    # land in the fat [32, 257) token bin (token-length chi-squared test).
    long_texts = [t for t in DRIFT_CORPUS if _word_count(t) >= 30]
    assert len(long_texts) / len(DRIFT_CORPUS) >= 0.80
