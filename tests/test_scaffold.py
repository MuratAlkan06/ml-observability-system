"""Scaffold sanity checks — keeps CI green and honest from PR #1.

Stdlib-only: asserts the frozen v1 plan and its cross-service frozen
constants are present in-repo, and that the MIT LICENSE survives the reset.
No project code exists yet (v1 in progress); this file exists so CI is both
green and meaningful before the first implementation slice lands.
"""

from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
PLAN = REPO_ROOT / "docs" / "PLAN.md"
LICENSE = REPO_ROOT / "LICENSE"

FROZEN_CONSTANTS = [
    "`mlobs:predictions` / `pg_writer` / `~ 50000`",
    "`distilbert-sst2-v1`",
    "Χ²>6.635 (df1) · Χ²>13.277 (df4) · KL>0.10 nats",
    "500 rows / 200 min / 60s",
    "900s per test",
]


def test_plan_exists():
    assert PLAN.is_file(), "docs/PLAN.md (frozen v1 spec) must exist"


def test_plan_has_frozen_constants_heading():
    text = PLAN.read_text(encoding="utf-8")
    assert "## Appendix A — cross-service frozen constants" in text


def test_plan_contains_frozen_constants():
    text = PLAN.read_text(encoding="utf-8")
    for const in FROZEN_CONSTANTS:
        assert const in text, f"frozen constant missing from docs/PLAN.md: {const!r}"


def test_license_present():
    assert LICENSE.is_file(), "LICENSE (MIT) must remain in the repo"
