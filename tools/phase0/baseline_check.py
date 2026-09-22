#!/usr/bin/env python3
"""Phase 0 baseline gate for SAOAF Issue #1 (I01).

Validates the machine-checkable close conditions of I01:
  1. docs/phase0/answers.md  - every mandatory question is ANSWERED (with
     evidence link, owner, date) or RISK-ACCEPTED (accepter, due date);
     P0 items may never sit in an intermediate state.
  2. docs/phase0/adr/*.md    - accepted ADRs have valid frontmatter
     (status, owner, date, supersedes chain intact).
  3. fixtures/mmr/           - profile snapshot fixture passes the schema
     rules from mocks/mmr/openapi.yaml; recorded calls carry both
     resource_plan_id and model_route_decision_id on every outcome.
  4. LICENSE decision        - docs/phase0/license-decision.md states the
     approved position; repo must NOT contain a LICENSE file unless the
     decision says one was granted.

Exit code 0 = gate passes; non-zero = gate fails (red run).
"""

from __future__ import annotations

import json
import re
import sys
from datetime import date
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
ANSWERS = REPO / "docs" / "phase0" / "answers.md"
ADR_DIR = REPO / "docs" / "adr"
FIXTURE_DIR = REPO / "fixtures" / "mmr"
LICENSE_DECISION = REPO / "docs" / "phase0" / "license-decision.md"
SNAPSHOT = FIXTURE_DIR / "profile-snapshot.json"
CALLS = FIXTURE_DIR / "recorded-calls.json"

TODAY = date.fromisoformat("2026-09-22")
VALID_STATES = {"ANSWERED", "RISK-ACCEPTED"}
P0_ALLOWED = {"ANSWERED", "RISK-ACCEPTED"}


def fail(msgs: list[str], msg: str) -> None:
    msgs.append(msg)


def parse_answers(msgs: list[str]) -> int:
    """Parse the mandatory-question ledger table; return count of checked rows."""
    if not ANSWERS.is_file():
        fail(msgs, f"missing ledger: {ANSWERS.relative_to(REPO)}")
        return 0
    text = ANSWS = ANSWERS.read_text(encoding="utf-8")
    rows = re.findall(
        r"^\|\s*(Q-[A-Z0-9-]+)\s*\|\s*(P0|P1|P2)\s*\|\s*([^|]+?)\s*\|\s*([A-Z-]+)\s*\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|",
        text,
        re.MULTILINE,
    )
    if not rows:
        fail(msgs, "answers ledger: no table rows parsed")
        return 0
    checked = 0
    for qid, prio, _topic, state, owner, evidence, due in rows:
        checked += 1
        if state not in VALID_STATES:
            fail(msgs, f"{qid}: intermediate/invalid state '{state}' (must be ANSWERED or RISK-ACCEPTED)")
            continue
        if not owner.strip() or owner.strip() in {"TBD", "TODO"}:
            fail(msgs, f"{qid}: missing owner")
        if evidence.strip() in {"-", "TBD", "TODO", "none"}:
            fail(msgs, f"{qid}: ANSWERED/RISK-ACCEPTED without evidence link")
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", due):
            fail(msgs, f"{qid}: missing ISO due/approval date")
        else:
            try:
                if date.fromisoformat(due) < TODAY and state == "RISK-ACCEPTED":
                    fail(msgs, f"{qid}: RISK-ACCEPTED due date {due} already passed")
            except ValueError:
                fail(msgs, f"{qid}: unparseable date {due}")
        if prio == "P0" and state not in P0_ALLOWED:
            fail(msgs, f"{qid}: P0 item not in terminal state")
    return checked


def check_adrs(msgs: list[str]) -> int:
    if not ADR_DIR.is_dir():
        fail(msgs, "missing docs/adr directory")
        return 0
    files = sorted(ADR_DIR.glob("adr-*.md"))
    if not files:
        fail(msgs, "no accepted ADRs found in docs/adr")
        return 0
    seen: dict[str, str] = {}
    count = 0
    for f in files:
        count += 1
        text = f.read_text(encoding="utf-8")
        fm = re.match(r"^---\n(.*?)\n---\n", text, re.DOTALL)
        if not fm:
            fail(msgs, f"{f.name}: missing frontmatter")
            continue
        fields = dict(
            re.findall(r"^(\w+):\s*(.+)$", fm.group(1), re.MULTILINE)
        )
        if fields.get("status") != "accepted":
            fail(msgs, f"{f.name}: status '{fields.get('status')}' not accepted")
        if not fields.get("owner") or fields.get("owner") in {"TODO", "TBD"}:
            fail(msgs, f"{f.name}: missing owner")
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", fields.get("decided", "")):
            fail(msgs, f"{f.name}: missing decided date")
        sup = fields.get("supersedes")
        if sup and sup != "-":
            for target in [s.strip() for s in sup.split(",")]:
                if target and target not in seen:
                    fail(msgs, f"{f.name}: supersedes unknown/forward ADR '{target}'")
        seen[f.stem] = fields.get("status", "")
    return count


SNAPSHOT_REQUIRED = {
    "provider_id", "snapshot_version", "contract_version", "generated_at",
    "valid_until", "profiles", "digest", "signature",
}
CALL_REQUIRED = {"model_route_decision_id", "resource_plan_id"}
OUTCOMES = {"SUCCEEDED", "PROFILE_NOT_FOUND", "QUOTA_EXCEEDED", "MODEL_UNAVAILABLE", "TIMEOUT"}


def check_fixtures(msgs: list[str]) -> int:
    if not SNAPSHOT.is_file():
        fail(msgs, f"missing fixture: {SNAPSHOT.relative_to(REPO)}")
        return 0
    snap = json.loads(SNAPSHOT.read_text(encoding="utf-8"))
    missing = SNAPSHOT_REQUIRED - set(snap)
    if missing:
        fail(msgs, f"snapshot fixture missing fields: {sorted(missing)}")
    if snap.get("provider_id") != "multi-model-router":
        fail(msgs, "snapshot fixture provider_id must be 'multi-model-router'")
    if not str(snap.get("digest", "")).startswith("sha256:"):
        fail(msgs, "snapshot fixture digest must be sha256-prefixed")
    if not snap.get("profiles"):
        fail(msgs, "snapshot fixture has no profiles")
    for p in snap.get("profiles", []):
        if not p.get("profile_id"):
            fail(msgs, "snapshot fixture profile without profile_id")
        if p.get("status") not in {"AVAILABLE", "DEGRADED", "UNAVAILABLE", "RETIRED"}:
            fail(msgs, f"snapshot fixture profile {p.get('profile_id')}: bad status")

    if not CALLS.is_file():
        fail(msgs, f"missing fixture: {CALLS.relative_to(REPO)}")
        return 1
    calls = json.loads(CALLS.read_text(encoding="utf-8"))
    if not isinstance(calls, list) or not calls:
        fail(msgs, "recorded-calls.json must be a non-empty array")
        return 1
    seen_outcomes = set()
    for c in calls:
        outcome = c.get("outcome")
        seen_outcomes.add(outcome)
        if outcome not in OUTCOMES:
            fail(msgs, f"recorded call: unknown outcome '{outcome}'")
        for field in CALL_REQUIRED:
            if not str(c.get(field, "")).strip():
                fail(msgs, f"recorded call ({outcome}): missing {field}")
        if c.get("resource_plan_id") and c.get("model_route_decision_id") \
                and c["resource_plan_id"] == c["model_route_decision_id"]:
            fail(msgs, f"recorded call ({outcome}): decision id must differ from plan id")
        if not c.get("case_id"):
            fail(msgs, f"recorded call: missing case_id")
    missing_outcomes = OUTCOMES - seen_outcomes
    if missing_outcomes:
        fail(msgs, f"recorded calls missing outcomes: {sorted(missing_outcomes)}")
    return len(calls)


def check_license(msgs: list[str]) -> None:
    if not LICENSE_DECISION.is_file():
        fail(msgs, "missing docs/phase0/license-decision.md")
        return
    text = LICENSE_DECISION.read_text(encoding="utf-8")
    fm = re.match(r"^---\n(.*?)\n---\n", text, re.DOTALL)
    if not fm:
        fail(msgs, "license-decision.md: missing frontmatter")
        return
    fields = dict(re.findall(r"^(\w+):\s*(.+)$", fm.group(1), re.MULTILINE))
    if fields.get("decision") not in {"granted", "deferred", "rejected"}:
        fail(msgs, "license-decision.md: decision must be granted|deferred|rejected")
    if not re.match(r"^\d{4}-\d{2}-\d{2}$", fields.get("decided", "")):
        fail(msgs, "license-decision.md: missing decided date")
    if not fields.get("owner") or fields.get("owner") in {"TODO", "TBD"}:
        fail(msgs, "license-decision.md: missing owner")
    has_license_file = (REPO / "LICENSE").exists()
    if fields.get("decision") in {"deferred", "rejected"} and has_license_file:
        fail(msgs, "LICENSE file present but decision says not granted")


def main() -> int:
    msgs: list[str] = []
    n_answers = parse_answers(msgs)
    n_adrs = check_adrs(msgs)
    n_calls = check_fixtures(msgs)
    check_license(msgs)
    if msgs:
        print("PHASE0 BASELINE GATE: FAIL")
        for m in msgs:
            print(f"  - {m}")
        return 1
    print("PHASE0 BASELINE GATE: PASS")
    print(f"  ledger rows checked: {n_answers}")
    print(f"  ADRs checked: {n_adrs}")
    print(f"  recorded calls checked: {n_calls}")
    print("  license decision: recorded")
    return 0


if __name__ == "__main__":
    sys.exit(main())
