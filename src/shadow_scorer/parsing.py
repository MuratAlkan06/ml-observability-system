"""Event-schema parsing/validation (docs/PLAN.md §3 field contract).

The shadow scorer joins the same ``mlobs:predictions`` stream as the primary
consumer, so it validates each entry against the identical §3 field contract.
Anything that fails raises ``MalformedEntry`` and is left unacked for the
recovery/poison path — exactly as in the primary consumer.

Per the repo rule (no cross-service imports; docs/PLAN.md §7) this validation is
consciously duplicated from ``src/consumer/parsing.py`` rather than imported.
``parse_event`` keeps only the fields the shadow path needs: the request_id / ts
identity and the *primary* half of the comparison (label + confidence), which are
denormalized into ``shadow_predictions`` for join-free agreement SQL.
"""

from __future__ import annotations

import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import datetime, timezone

# INSERT column order for shadow_predictions — the sole source of truth for the
# record-tuple layout the scorer builds.
SHADOW_COLUMNS = (
    "request_id",
    "ts",
    "model_version",
    "label",
    "confidence",
    "token_count",
    "latency_ms",
    "primary_label",
    "primary_confidence",
)

_LABELS = frozenset({"positive", "negative"})


class MalformedEntry(ValueError):
    """Raised when a stream entry violates the §3 field contract."""


@dataclass(frozen=True)
class PrimaryEvent:
    """The validated subset of a §3 event the shadow scorer consumes.

    ``primary_label`` / ``primary_confidence`` are the primary model's output,
    copied verbatim from the event so the comparison needs no join back to the
    ``predictions`` table (and no race against the pg_writer commit).
    """

    request_id: uuid.UUID
    ts: datetime
    text: str
    primary_label: str
    primary_confidence: float


def parse_event(fields: Mapping[str, str]) -> PrimaryEvent:
    """Validate a flat field map against the §3 contract; return a PrimaryEvent.

    Every §3 field is validated (identical rules to the primary consumer) so a
    malformed entry is detected the same way here; only the fields the shadow
    path needs are returned. Raises MalformedEntry on any contract violation.
    """
    try:
        request_id = uuid.UUID(str(fields["request_id"]))

        ts_ms = int(fields["ts_ms"])
        ts = datetime.fromtimestamp(ts_ms / 1000, tz=timezone.utc)

        text = str(fields["text"])
        if not 1 <= len(text) <= 1000:
            raise MalformedEntry(f"text length {len(text)} out of [1, 1000]")

        token_count = int(fields["token_count"])
        if not 3 <= token_count <= 256:
            raise MalformedEntry(f"token_count {token_count} out of [3, 256]")

        label = str(fields["label"])
        if label not in _LABELS:
            raise MalformedEntry(f"label {label!r} not in {sorted(_LABELS)}")

        confidence = float(fields["confidence"])
        if not 0.0 <= confidence <= 1.0:
            raise MalformedEntry(f"confidence {confidence} out of [0.0, 1.0]")

        model_version = str(fields["model_version"])
        if not model_version:
            raise MalformedEntry("model_version is empty")

        latency_ms = float(fields["latency_ms"])
        if latency_ms < 0.0:
            raise MalformedEntry(f"latency_ms {latency_ms} < 0.0")
    except MalformedEntry:
        raise
    except (KeyError, ValueError, TypeError) as exc:
        raise MalformedEntry(str(exc)) from exc

    return PrimaryEvent(
        request_id=request_id,
        ts=ts,
        text=text,
        primary_label=label,
        primary_confidence=confidence,
    )
