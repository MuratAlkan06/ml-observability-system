"""Input corpora for the traffic simulator.

Two corpora, selected by ``--mode`` (docs/PLAN.md §5, §7). Both were validated
against the baked ``distilbert-sst2-v1`` model and the frozen
``baseline/baseline.json`` reference (the E2E measurement in S5): every corpus
text is <=1000 characters with >=1 non-whitespace character (the API input
contract, docs/PLAN.md §2), and the two corpora sit on opposite sides of the
three §5 drift thresholds when they saturate the latest-500 window.

* ``NORMAL_CORPUS`` — clearly polarized, roughly class-balanced SST-2-style
  movie reviews spanning the full range of lengths so the production window
  tracks the frozen baseline on all three axes and NO drift test fires:
  class chi-squared ~1 (<6.635), token-length chi-squared ~6 (<13.277), and
  confidence KL ~0.089 (<0.10). Length matters: the reviews are deliberately
  spread across all five token-length bins (a few very short, most medium,
  many long) to mirror the baseline's ``token_len_probs`` — a corpus of only
  short reviews would fire the token-length test even on "normal" traffic.

* ``DRIFT_CORPUS`` — long (>=33 tokens each), negative-skewed, hedged and
  ambiguous out-of-domain text engineered to trip all three §5 tests inside a
  single 500-row window simultaneously:
    1. class chi-squared (>6.635, df1) — heavily negative-skewed vs the ~47/53
       baseline (stat ~558);
    2. token-length chi-squared (>13.277, df4) — long enough to flood the fat
       ``[32, 257)`` token bin, DistilBERT tokenization incl. [CLS]/[SEP],
       max_length=256 (stat ~1382);
    3. confidence KL (>0.10 nats) — a deliberate mix of strongly-negative and
       genuinely ambiguous (mixed-sentiment / neutral) reviews pushes softmax
       confidence mass DOWN out of the 0.95-1.0 bin where polarized SST-2 text
       concentrates (KL ~0.198). DistilBERT is highly confident even on long
       bureaucratic prose, so the ambiguous, praise-then-pan reviews are what
       actually move confidence below threshold.
"""

from __future__ import annotations

NORMAL_CORPUS: list[str] = [
    "Absolutely magnificent.",
    "Cheap effects and wooden performances ruin this dreadful bore.",
    "An utterly forgettable film with no redeeming qualities whatsoever.",
    "I hated every minute of this awful, poorly written disaster.",
    "An unforgettable, delightful adventure that thrilled me completely.",
    "Wonderful, witty, and wildly entertaining from the very first scene.",
    "The screenplay is sharp, funny, and genuinely inspiring.",
    "The acting was superb and the direction was flawless throughout.",
    "A dreadful, tedious mess that insults the intelligence of its audience.",
    "A boring, pretentious failure that I deeply regret ever watching.",
    "A dull, clumsy, poorly written mess that wastes a very good cast on a terrible script.",
    "One of the worst films I have ever been forced to sit through.",
    "The dialogue was cringeworthy and the story made absolutely no sense.",
    "A charming, tender, wonderfully acted little comedy that left me smiling from beginning to happy end.",
    "A sharp, funny, deeply satisfying film that had the whole audience laughing and cheering throughout.",
    "A gorgeous, moving triumph that I will happily watch again and again.",
    "A brilliant, heartfelt performance that left me in tears of joy.",
    "An elegant, moving, gorgeously shot drama anchored by two of the finest performances of the year.",
    "Stunning visuals and a gripping story make this a must-see triumph.",
    "Cheap effects, wooden acting, and a lazy script make this one of the dullest films of the entire year.",
    "A tedious, poorly acted slog that squanders a promising premise and an obviously talented ensemble cast.",
    "A dreary, lifeless bore that lurches from one flat, charmless scene to the next without a shred of real tension.",
    "A lazy, cynical, joyless slog with no wit, no tension, and no real reason to exist.",
    "A lazy, cynical misfire that wastes a fine cast on a witless script and painfully limp, uninspired, joyless direction.",
    "A delightful, big-hearted charmer, funny and touching in equal measure, that sends its audience home grinning from ear to ear.",
    "A warm and generous film, beautifully acted and gorgeously shot, that earns every laugh and every tear it draws from its grateful audience.",
    "A sharp, witty, wonderfully inventive comedy that kept the entire audience laughing from the very first scene to the last.",
    "A warm, generous, beautifully acted film that earns every laugh and every tear it so effortlessly draws out.",
    "Warm, funny, and genuinely moving, it is the rare crowd-pleaser that actually deserves the applause it gets.",
    "A gorgeous, moving, superbly acted triumph that swept me up completely and left me quietly cheering at the end.",
    "A witless, charmless, punishingly loud disaster with no discernible plot and no likable characters, and I resented nearly every tedious and wildly overlong minute of it.",
    "Cheap, cynical, and utterly charmless, this dreary slog lurches from one lifeless scene to the next without ever generating a single moment of genuine tension, humor, or honest emotional truth.",
    "A lumbering, humorless bore that squanders its talented cast on a witless script, limp direction, and a story so predictable that I had guessed the ending within the first ten minutes.",
    "This is easily the worst film I have endured in years, a witless, charmless, punishingly loud disaster with no discernible plot, no likable characters, and no ambition beyond assaulting the senses, and I resented every single tedious, insulting, wildly overlong minute of the whole miserable ordeal from start to finish.",
    "A grim and pretentious failure, airless and self-important, that mistakes tedium for depth and leaves its talented cast stranded with absolutely nothing worthwhile to do.",
    "Every performance is pitch perfect, the script crackles with wit and warmth, and the direction is so confident and assured that the whole film feels like an instant timeless classic.",
    "An exhilarating, wonderfully surprising adventure, full of wit and heart, that delivers spectacular thrills and real emotional depth in almost equal and thoroughly satisfying measure.",
    "From its opening frames to its rousing finale this is a warm, generous, beautifully acted film that earns every ounce of the joy and emotion it so effortlessly delivers.",
    "A stirring, gorgeously mounted triumph, marvelously acted and deeply felt, that had me grinning through happy tears from its first scene to its glorious final closing shot.",
    "This is, without the slightest hesitation, one of the most stirring and gorgeously mounted films I have seen in years, a sweeping, tender, marvelously acted triumph that had me grinning through happy tears from its very first scene straight through to its glorious and deeply satisfying conclusion.",
]

DRIFT_CORPUS: list[str] = [
    "There is intelligence here, and ambition, and a few scenes of startling power, yet they float in a sea of longueurs and misjudged comic beats, and I left admiring individual pieces of the thing while feeling almost nothing at all about the whole.",
    "I found it by turns dazzling and tiresome, moving and mechanical, genuinely funny and painfully strained, and this ceaseless oscillation between real accomplishment and obvious miscalculation left me unable to decide whether I had enjoyed myself or merely endured the experience.",
    "There are flashes of brilliance scattered throughout, a witty exchange here, a breathtaking image there, and I kept waiting for them to cohere into something greater, but they never quite did, and what remained was handsome, ambitious, and finally rather hollow.",
    "The craft is undeniable, the ambition admirable, and a handful of scenes achieve a startling, aching beauty, yet the whole remains stubbornly less than the sum of its gleaming parts, admirable in the abstract and strangely unmoving in the moment.",
    "Neither the triumph its admirers claim nor the fiasco its detractors insist upon, it is instead a curious, uneven, occasionally lovely and frequently frustrating film that left me genuinely unsure whether I had witnessed something flawed and ambitious or merely handsome and empty.",
    "Alternately gorgeous and ungainly, tender and pretentious, sharply funny and tediously overwrought, the film never settles into a coherent identity, and by the end I admired individual pieces of it while feeling almost nothing at all about the exhausting whole.",
    "It is competent enough, I suppose, and a few performances rise above the material, yet nothing about this cautious, middling, oddly passionless picture lingers in the memory, and within an hour of leaving the theater I could scarcely recall a single distinct scene.",
    "It has genuine virtues, real ones, a luminous lead performance and a few sequences of considerable power, but they are marooned in a sprawling, undisciplined, self-regarding film that mistakes length for depth and solemnity for genuine emotional weight.",
    "It is not without its virtues, the score is lovely and a handful of scenes genuinely land, but the sluggish pacing, the muddled screenplay, and the strangely inert direction gradually smother whatever promise the material might once have had.",
    "It aspires to profundity and occasionally brushes against it, yet just as often it topples into pretension, and this constant wobble between the genuinely moving and the faintly ridiculous makes for a frustrating, curiously weightless couple of hours at the cinema.",
    "Beautifully mounted and often quite affecting, it is also overlong, self-important, and curiously inert, so that every stretch of genuine grace is followed almost immediately by another of airless, ponderous, thoroughly avoidable tedium that steadily erodes one's goodwill.",
    "There is wit here, and warmth, and moments of real visual splendor, but they are scattered so unevenly across such a long, meandering, self-satisfied running time that the cumulative effect is less exhilaration than a kind of patient, low-grade exasperation.",
    "The medical device recall notice is phrased with such extraordinary caution that patients are left genuinely unsure whether they should discontinue use immediately, schedule a consultation, or simply monitor for symptoms, and the accompanying support hotline has been so overwhelmed that obtaining any clarifying guidance has proven nearly impossible.",
    "There are moments of real beauty scattered throughout, and the lead performance is undeniably committed, yet the film is so tonally erratic and structurally confused that these fleeting pleasures never coalesce into anything remotely satisfying or coherent.",
    "The performances range from genuinely affecting to distractingly mannered, the tone lurches between earnest and glib, and while it is never quite boring it is also never quite convincing, leaving a vaguely dissatisfied aftertaste that lingers far longer than the film itself.",
    "For all its evident craft and occasional flashes of brilliance, the film is finally undone by its own excess, a sprawling, self-serious, emotionally distant work that inspires admiration far more readily than affection and exhaustion far more readily than either.",
    "I wanted to love it, and for stretches I nearly did, yet the longer it went on the more its flaws accumulated, until the graceful early promise curdled into something muddled, overlong, and faintly exasperating that left me more puzzled than genuinely moved.",
    "The property inspection summary flags a number of minor-to-moderate concerns, including questionable wiring, uneven settling, and possible moisture intrusion, yet it repeatedly emphasizes that further specialized evaluation would be necessary before any of these somewhat worrying observations could be definitively characterized as serious structural or safety deficiencies.",
    "I can see why some viewers respond to it, and there is undeniable skill on display, but the relentless bleakness, the airless pacing, and the smugly withheld emotion left me cold, detached, and increasingly impatient for the whole solemn ordeal to end.",
    "The vendor's incident postmortem concedes that the prolonged outage was regrettable, yet it conspicuously avoids assigning any concrete cause, instead gesturing vaguely toward a cascade of contributing factors that, taken together, explain remarkably little and leave affected customers with scant assurance that the same failure will not simply recur.",
    "The travel advisory for the region is couched in such carefully noncommittal language that it is nearly impossible to gauge the actual level of risk, oscillating between reassurance and caution from one paragraph to the very next, and ultimately leaving the prospective traveler no better informed than they were before consulting it.",
    "The community garden coordination email laments that the shared tool shed has once again been left in complete disarray, with several missing implements and an unexplained broken lock, and while no one wishes to point fingers, the lingering ambiguity has quietly soured the atmosphere among the otherwise amicable volunteers.",
    "There is a decent, even affecting film buried somewhere inside this bloated, meandering, tonally uncertain one, but it is so obscured by needless subplots and self-indulgent flourishes that recovering it requires far more patience than most reasonable viewers will possess.",
    "The insurance adjuster's preliminary assessment is so hedged with qualifications and conditional clauses that it remains genuinely unclear whether the water damage claim will be approved, denied, or endlessly deferred, and repeated attempts to obtain a straight answer have so far yielded nothing but polite, noncommittal deflection.",
]


def get_corpus(mode: str) -> list[str]:
    """Return the corpus for ``mode`` (``"normal"`` or ``"drift"``)."""
    if mode == "normal":
        return NORMAL_CORPUS
    if mode == "drift":
        return DRIFT_CORPUS
    raise ValueError(f"unknown mode: {mode!r}")
