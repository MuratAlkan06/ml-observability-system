"""Input corpora for the traffic simulator.

Two corpora, selected by ``--mode`` (docs/PLAN.md §5, §7):

* ``NORMAL_CORPUS`` — short, clearly polarized SST-2-style movie reviews,
  roughly balanced positive/negative. High-confidence predictions land in the
  top confidence bin and short texts land in the low token-length bins, so the
  production window tracks the frozen baseline and no drift test fires.

* ``DRIFT_CORPUS`` — long (30+ words each), negative-leaning, hedged and
  out-of-domain text, engineered to trip all three §5 tests inside a single
  500-row window simultaneously:
    1. class chi-squared (>6.635, df1) — heavily negative-skewed sentiment vs
       the ~51/49 baseline;
    2. token-length chi-squared (>13.277, df4) — long enough to flood the fat
       ``[32, 257)`` token bin (DistilBERT tokenization incl. [CLS]/[SEP],
       max_length=256);
    3. confidence KL (>0.10 nats) — neutral/ambiguous/out-of-domain phrasing
       pushes softmax confidence mass down out of the 0.95-1.0 bin where
       polarized SST-2 text concentrates.

Every text is <=1000 characters and has >=1 non-whitespace character so the API
input contract (docs/PLAN.md §2) never rejects it.
"""

from __future__ import annotations

NORMAL_CORPUS: list[str] = [
    # Positive — clearly polarized, high confidence.
    "This film is an absolute masterpiece from start to finish.",
    "A brilliant, heartfelt performance that left me in tears of joy.",
    "One of the best movies I have ever had the pleasure of watching.",
    "Stunning visuals and a gripping story make this a must-see triumph.",
    "The acting was superb and the direction was flawless throughout.",
    "An unforgettable, delightful adventure that thrilled me completely.",
    "Wonderful, witty, and wildly entertaining from the very first scene.",
    "A gorgeous, moving triumph that I will happily watch again and again.",
    "The screenplay is sharp, funny, and genuinely inspiring.",
    "Pure cinematic magic; I loved every single minute of it.",
    # Negative — clearly polarized, high confidence.
    "This movie was a complete disaster and a total waste of time.",
    "Dull, lifeless, and painfully boring from beginning to end.",
    "One of the worst films I have ever been forced to sit through.",
    "The plot was incoherent and the acting was absolutely terrible.",
    "A dreadful, tedious mess that insults the intelligence of its audience.",
    "I hated every minute of this awful, poorly written disaster.",
    "Cheap effects and wooden performances ruin this dreadful bore.",
    "An utterly forgettable film with no redeeming qualities whatsoever.",
    "The dialogue was cringeworthy and the story made absolutely no sense.",
    "A boring, pretentious failure that I deeply regret ever watching.",
]

DRIFT_CORPUS: list[str] = [
    "The quarterly logistics reconciliation report suggests that several intermediate distribution nodes may possibly have experienced some intermittent throughput degradation, though the underlying causes remain rather unclear and the operational teams have not yet offered any conclusive explanation for the recurring anomalies observed across the affected regional warehouses this period.",
    "According to the preliminary compliance memorandum, it appears that certain procedural checkpoints might not have been fully documented, which could conceivably indicate a lapse in oversight, although management maintains that the situation is somewhat ambiguous and that further review will probably be required before any firm conclusions can reasonably be drawn.",
    "The homeowner association newsletter notes that the perpetually malfunctioning irrigation controller has once again failed to activate on schedule, leaving several common areas parched and neglected, and residents are understandably frustrated, though nobody seems entirely certain whether the fault lies with the aging hardware or the recently updated firmware.",
    "Our internal survey results are frankly difficult to interpret, since respondents expressed a confusing mixture of mild dissatisfaction and cautious indifference, and while the overall tone leans somewhat negative, the sample size is arguably too small to support any sweeping generalization about employee morale across the wider organization at this time.",
    "The appliance warranty documentation is written in such convoluted, bureaucratic language that it is genuinely hard to tell whether the intermittent compressor failure would even be covered, and the customer service representative I spoke with seemed just as uncertain, offering only vague reassurances and no actual commitment to any repair.",
    "The municipal water treatment status update indicates that turbidity levels have been fluctuating unpredictably over the past several weeks, and while officials insist there is no immediate cause for alarm, the report itself acknowledges that the monitoring equipment has been unreliable and that some of the readings may be inaccurate or incomplete.",
    "I attempted to follow the assembly instructions for the flat-pack cabinet, but the diagrams were so poorly labeled and the hardware bags so mysteriously mismatched that after nearly three hours of mounting frustration I still cannot say with any confidence whether the finished result is structurally sound or dangerously unstable.",
    "The software release notes vaguely mention that a number of unspecified issues have been partially addressed, yet several long-standing defects apparently persist, and the changelog is so terse and noncommittal that it is nearly impossible to determine whether upgrading would actually improve the situation or merely introduce a fresh set of complications.",
    "The regional transit authority's service advisory admits that delays have become somewhat routine, though it stops short of explaining why, and commuters are left to speculate whether the recurring signal faults, the aging rolling stock, or simple mismanagement is chiefly responsible for the deteriorating and increasingly unpredictable schedule.",
    "The insurance adjuster's preliminary assessment is so hedged with qualifications and conditional clauses that it remains genuinely unclear whether the water damage claim will be approved, denied, or endlessly deferred, and repeated attempts to obtain a straight answer have so far yielded nothing but polite, noncommittal deflection.",
    "The laboratory's equipment maintenance log records a troubling pattern of intermittent calibration drift across multiple instruments, but the accompanying notes are inconsistent and occasionally contradictory, making it difficult to establish whether the anomalies stem from operator error, environmental fluctuation, or a more serious and systematic underlying malfunction.",
    "The tenant complaint form describes a persistent, unidentifiable odor emanating from somewhere within the walls, and although two separate inspectors have already visited the unit, neither could locate the source nor rule out the possibility of a hidden leak, leaving the increasingly exasperated resident with far more questions than answers.",
    "The annual budget variance commentary attributes the disappointing shortfall to a vague constellation of external headwinds and unforeseen operational frictions, but the explanation is so generic and evasive that it provides almost no actionable insight into what specifically went wrong or how similar disappointments might plausibly be avoided in future.",
    "The firmware update advisory cautiously warns that some devices may experience reduced battery performance, unexpected reboots, or degraded connectivity following installation, and while the manufacturer conspicuously downplays the severity, the sheer length of the accompanying list of known issues does very little to inspire genuine confidence in the release.",
    "The property inspection summary flags a number of minor-to-moderate concerns, including questionable wiring, uneven settling, and possible moisture intrusion, yet it repeatedly emphasizes that further specialized evaluation would be necessary before any of these somewhat worrying observations could be definitively characterized as serious structural or safety deficiencies.",
    "The customer feedback aggregation for this quarter paints a decidedly muddled picture, with lukewarm praise awkwardly interleaved with pointed criticism, and although the net sentiment appears to tilt mildly toward disappointment, the responses are so internally inconsistent that drawing any confident conclusion feels frankly premature and possibly misleading.",
    "The road maintenance bulletin acknowledges that the recurring potholes along the northern corridor have not been adequately repaired despite repeated resurfacing attempts, and it offers only a tentative timeline for future work, hedged with the usual caveats about weather, funding, and competing infrastructure priorities elsewhere across the district.",
    "The medical device recall notice is phrased with such extraordinary caution that patients are left genuinely unsure whether they should discontinue use immediately, schedule a consultation, or simply monitor for symptoms, and the accompanying support hotline has been so overwhelmed that obtaining any clarifying guidance has proven nearly impossible.",
    "The vendor's incident postmortem concedes that the prolonged outage was regrettable, yet it conspicuously avoids assigning any concrete cause, instead gesturing vaguely toward a cascade of contributing factors that, taken together, explain remarkably little and leave affected customers with scant assurance that the same failure will not simply recur.",
    "The community garden coordination email laments that the shared tool shed has once again been left in complete disarray, with several missing implements and an unexplained broken lock, and while no one wishes to point fingers, the lingering ambiguity has quietly soured the atmosphere among the otherwise amicable volunteers.",
    "The archived meeting minutes are so fragmentary and riddled with unresolved action items that it is genuinely difficult to reconstruct what, if anything, was actually decided, and the few conclusions that can be inferred seem tentative, provisional, and rather likely to be revisited or quietly abandoned at some later date.",
    "The utility company's rate adjustment notification buries its most consequential changes beneath dense layers of technical qualification, and after reading it several times over I remain honestly uncertain whether my monthly costs will rise substantially, marginally, or perhaps not at all, depending on factors that are never quite clearly specified.",
    "The travel advisory for the region is couched in such carefully noncommittal language that it is nearly impossible to gauge the actual level of risk, oscillating between reassurance and caution from one paragraph to the very next, and ultimately leaving the prospective traveler no better informed than they were before consulting it.",
    "The recurring server monitoring digest reports a steady trickle of low-priority warnings that never quite escalate into anything actionable, yet also never fully resolve, and this persistent background noise has made it increasingly difficult for the on-call engineers to distinguish a genuine emerging problem from the usual ambient dysfunction.",
]


def get_corpus(mode: str) -> list[str]:
    """Return the corpus for ``mode`` (``"normal"`` or ``"drift"``)."""
    if mode == "normal":
        return NORMAL_CORPUS
    if mode == "drift":
        return DRIFT_CORPUS
    raise ValueError(f"unknown mode: {mode!r}")
