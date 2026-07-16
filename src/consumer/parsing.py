"""Event-schema parsing/validation (docs/PLAN.md §3 field contract).

Every stream entry is a flat string map. ``parse_entry`` validates it against
the §3 contract and the §4 DDL CHECK constraints and returns a positional
record tuple ready for the batch INSERT. Anything that fails raises
``MalformedEntry`` so the consumer can skip it without aborting the batch
(a bad row in a multi-statement transaction would otherwise roll back the
whole batch).
"""

from __future__ import annotations

import uuid
from collections.abc import Mapping
from datetime import datetime, timezone

# INSERT column order — the sole source of truth for record-tuple layout.
COLUMNS = (
    "request_id",
    "ts",
    "text",
    "token_count",
    "label",
    "confidence",
    "model_version",
    "latency_ms",
)

_LABELS = frozenset({"positive", "negative"})


class MalformedEntry(ValueError):
    """Raised when a stream entry violates the §3/§4 contract."""


def parse_entry(fields: Mapping[str, str]) -> tuple:
    """Validate a flat field map and return an insert-ready record tuple.

    Raises MalformedEntry on any contract violation.
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

    return (
        request_id,
        ts,
        text,
        token_count,
        label,
        confidence,
        model_version,
        latency_ms,
    )
