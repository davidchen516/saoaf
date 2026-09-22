#!/usr/bin/env python3
"""Targeted tests for tools/phase0/baseline_check.py (review findings F1/F3/F6).

Each test imports the gate module against a /tmp copy of the repo docs so the
real repository files are never modified. Run: python3 tests/phase0/test_baseline_check.py
Exit 0 = all tests pass.
"""

from __future__ import annotations

import importlib.util
import re
import shutil
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

PASSED: list[str] = []
FAILED: list[str] = []


def _load_gate(gate_src: str, root: Path, today_env: str | None = None):
    """Load a fresh gate module whose REPO points at root; optional SAOAF_TODAY injected."""
    import os
    if today_env is not None:
        os.environ["SAOAF_TODAY"] = today_env
    try:
        spec = importlib.util.spec_from_file_location(
            f"baseline_check_{abs(hash((str(root), today_env))) & 0xffffff:x}",
            root / "tools" / "phase0" / "baseline_check.py",
        )
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        mod.REPO = root
        for name, rel in [
            ("ANSWERS", "docs/phase0/answers.md"),
            ("ADR_DIR", "docs/adr"),
            ("FIXTURE_DIR", "fixtures/mmr"),
            ("LICENSE_DECISION", "docs/phase0/license-decision.md"),
            ("SNAPSHOT", "fixtures/mmr/profile-snapshot.json"),
            ("CALLS", "fixtures/mmr/recorded-calls.json"),
        ]:
            setattr(mod, name, root / rel)
        return mod
    finally:
        if today_env is not None:
            del os.environ["SAOAF_TODAY"]


def make_gate(tmp: Path, *, today_env: str | None = None):
    """Materialize a gate instance whose REPO points at a /tmp copy."""
    root = tmp / "repo"
    for d in ("docs", "tools", "fixtures", "mocks"):
        shutil.copytree(REPO / d, root / d, dirs_exist_ok=True)
    return _load_gate(None, root, today_env)


def check(name: str, cond: bool, detail: str = "") -> None:
    (PASSED if cond else FAILED).append(f"{name}{(' — ' + detail) if detail and not cond else ''}")


def test_f1_overdue_fails_with_future_clock() -> None:
    """F1: with a simulated clock past 2026-10-31, overdue RISK-ACCEPTED must FAIL."""
    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td), today_env="2026-11-15")
        msgs: list[str] = []
        gate.parse_answers(msgs)
        overdue = [m for m in msgs if "already passed" in m]
        check("F1 overdue flagged (Q-MMR-02)", any("Q-MMR-02" in m for m in overdue), str(overdue))
        check("F1 overdue flagged (Q-CAP-01)", any("Q-CAP-01" in m for m in overdue))
        check("F1 no false positive on ANSWERED rows", not any("Q-LANG-01" in m for m in msgs))


def test_f1_today_follows_real_clock() -> None:
    """F1: without SAOAF_TODAY, TODAY must equal the real system date."""
    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td))
        from datetime import date as _date
        check("F1 TODAY == date.today()", gate.TODAY == _date.today(), f"gate.TODAY={gate.TODAY}")


def test_f3_superseded_lifecycle() -> None:
    """F3: superseded ADR must be accepted AND must point to its successor."""
    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td))
        adr_dir = gate.ADR_DIR
        # a compliant supersede: adr-0001 -> superseded with superseded-by, adr-0006 accepted
        (adr_dir / "adr-0001-language-go.md").write_text(
            (adr_dir / "adr-0001-language-go.md").read_text(encoding="utf-8").replace(
                "status: accepted", "status: superseded\nsuperseded-by: adr-0006-successor"
            ), encoding="utf-8")
        (adr_dir / "adr-0006-successor.md").write_text(
            "---\nadr: ADR-PHASE0-0006\ntitle: successor\nstatus: accepted\n"
            "owner: David\ndecided: 2026-11-01\nsupersedes: adr-0001-language-go\n---\n# successor\n",
            encoding="utf-8")
        msgs: list[str] = []
        n = gate.check_adrs(msgs)
        check("F3 compliant supersede passes", not msgs, "; ".join(msgs))
        check("F3 six ADRs counted", n == 6, f"n={n}")

    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td))
        adr_dir = gate.ADR_DIR
        # non-compliant: superseded WITHOUT successor link -> must fail
        (adr_dir / "adr-0001-language-go.md").write_text(
            (adr_dir / "adr-0001-language-go.md").read_text(encoding="utf-8").replace(
                "status: accepted", "status: superseded"
            ), encoding="utf-8")
        msgs: list[str] = []
        gate.check_adrs(msgs)
        check("F3 superseded without successor fails", any("superseded" in m for m in msgs), "; ".join(msgs))

    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td))
        adr_dir = gate.ADR_DIR
        # non-compliant: superseded pointing at a NON-EXISTENT successor -> must fail
        (adr_dir / "adr-0001-language-go.md").write_text(
            (adr_dir / "adr-0001-language-go.md").read_text(encoding="utf-8").replace(
                "status: accepted", "status: superseded\nsuperseded-by: adr-9999-ghost"
            ), encoding="utf-8")
        msgs: list[str] = []
        gate.check_adrs(msgs)
        check("F3 superseded pointing to ghost successor fails", any("ghost" in m or "superseded" in m for m in msgs), "; ".join(msgs))


def test_f6_duplicate_qid_fails() -> None:
    """F6: two conflicting ledger rows with the same Q-ID must FAIL."""
    with tempfile.TemporaryDirectory() as td:
        gate = make_gate(Path(td))
        ledger = gate.ANSWERS
        text = ledger.read_text(encoding="utf-8")
        # append a conflicting duplicate row right after the original
        dup = re.sub(
            r"(\| Q-LANG-01 \| P0 \| 控制面主语言 \| ANSWERED \| David \| \[adr-0001\]\(\.\./adr/adr-0001-language-go\.md\) \| 2026-09-22 \|)",
            r"\1\n| Q-LANG-01 | P0 | 控制面主语言（冲突回答 B）| ANSWERED | David | [adr-0003](../adr/adr-0003-nats.md) | 2026-09-22 |",
            text, count=1)
        ledger.write_text(dup, encoding="utf-8")
        msgs: list[str] = []
        gate.parse_answers(msgs)
        check("F6 duplicate Q-ID detected", any("duplicate" in m.lower() or "重复" in m for m in msgs), "; ".join(msgs))


def main() -> int:
    test_f1_overdue_fails_with_future_clock()
    test_f1_today_follows_real_clock()
    test_f3_superseded_lifecycle()
    test_f6_duplicate_qid_fails()
    print(f"PASS: {len(PASSED)}  FAIL: {len(FAILED)}")
    for p in PASSED:
        print(f"  ok   {p}")
    for f in FAILED:
        print(f"  FAIL {f}")
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
