#!/usr/bin/env python3
"""Replay the MMR contract fixtures against a live Prism mock (Issue #1).

Starts mocks/mmr via docker compose, replays every case in
fixtures/mmr/recorded-calls.json as a real HTTP call, and asserts:
  - expected HTTP status matches;
  - response carries both resource_plan_id and model_route_decision_id
    (body for success, error envelope for failures);
  - the profile snapshot fixture round-trips against
    GET /control/v1/profile-snapshots/current and satisfies the schema rules.

Exit code 0 = replay passes; non-zero = replay fails (red run).
"""

from __future__ import annotations

import json
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
COMPOSE = REPO / "mocks" / "mmr" / "compose.yaml"
FIXTURES = REPO / "fixtures" / "mmr"
BASE = "http://127.0.0.1:4020"

PROJECT = "saoaf-mmr-fixture"


def sh(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(args, capture_output=True, text=True, check=check)


def start_mock() -> None:
    sh("docker", "compose", "-p", PROJECT, "-f", str(COMPOSE), "up", "-d", "--wait")
    print("prism mock: up")


def stop_mock() -> None:
    sh("docker", "compose", "-p", PROJECT, "-f", str(COMPOSE), "down", "-v", check=False)
    print("prism mock: down")


def http_json(method: str, path: str, headers: dict, body: dict | None = None):
    req = urllib.request.Request(BASE + path, method=method)
    for k, v in headers.items():
        req.add_header(k, v)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, data, timeout=15) as resp:
            raw = resp.read()
            return resp.status, dict(resp.headers), json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = {"_raw": raw.decode(errors="replace")}
        return e.code, dict(e.headers), parsed


def wait_for_mock(retries: int = 30) -> None:
    for _ in range(retries):
        try:
            status, _, _ = http_json("GET", "/v1/models", {})
            if status == 200:
                return
        except Exception:
            pass
        time.sleep(1)
    raise RuntimeError("prism mock did not become ready")


def replay_snapshot_fixture(fails: list[str]) -> None:
    """GET the snapshot endpoint and cross-check the stored fixture."""
    fixture = json.loads((FIXTURES / "profile-snapshot.json").read_text())
    status, _, body = http_json("GET", "/control/v1/profile-snapshots/current", {})
    if status != 200:
        fails.append(f"snapshot endpoint returned {status}")
        return
    for key in ("provider_id", "snapshot_version", "contract_version", "digest", "signature"):
        if key not in body:
            fails.append(f"live snapshot missing field {key}")
    if body.get("provider_id") != fixture["provider_id"]:
        fails.append("live snapshot provider_id differs from fixture")
    if not str(body.get("digest", "")).startswith("sha256:"):
        fails.append("live snapshot digest not sha256-prefixed")
    profiles = {p.get("profile_id") for p in body.get("profiles", [])}
    for p in fixture["profiles"]:
        if p["profile_id"] not in profiles:
            fails.append(f"fixture profile {p['profile_id']} not present in live snapshot")
    print(f"snapshot fixture: OK (provider={body.get('provider_id')}, digest={body.get('digest')})")


def replay_calls(fails: list[str]) -> int:
    cases = json.loads((FIXTURES / "recorded-calls.json").read_text())
    for case in cases:
        h = case["http"]
        status, headers, body = http_json(h["method"], h["path"], h["headers"], h.get("body"))
        cid = case["case_id"]
        if status != case["expected_status"]:
            fails.append(f"{cid}: expected {case['expected_status']}, got {status}")
            continue
        # Correlation IDs must be present on every outcome. Prism is a static
        # example mock: it returns the recorded example IDs rather than echoing
        # request headers, so we assert presence + distinctness, not equality
        # with the sent request headers (that is a real-MMR integration check,
        # gated on OPEN-02).
        plan = body.get("resource_plan_id") or headers.get("X-Resource-Plan-ID")
        decision = body.get("model_route_decision_id") or headers.get("X-Model-Route-Decision-ID")
        if not plan:
            fails.append(f"{cid}: response carries no resource_plan_id")
        if not decision:
            fails.append(f"{cid}: response carries no model_route_decision_id")
        if case.get("expected_error_code"):
            code = (body.get("error") or {}).get("code")
            if code != case["expected_error_code"]:
                fails.append(f"{cid}: expected error code {case['expected_error_code']}, got {code}")
        if plan and decision and plan == decision:
            fails.append(f"{cid}: decision id equals plan id")
        print(f"{cid}: {case['outcome']} status={status} plan={plan} decision={decision}")
    return len(cases)


def main() -> int:
    fails: list[str] = []
    started = False
    try:
        start_mock()
        started = True
        wait_for_mock()
        replay_snapshot_fixture(fails)
        n = replay_calls(fails)
    finally:
        if started:
            stop_mock()
    if fails:
        print("MMR FIXTURE REPLAY: FAIL")
        for f in fails:
            print(f"  - {f}")
        return 1
    print("MMR FIXTURE REPLAY: PASS")
    print(f"  cases replayed: {n} (5 outcomes: SUCCEEDED/PROFILE_NOT_FOUND/QUOTA_EXCEEDED/MODEL_UNAVAILABLE/TIMEOUT-class)")
    print("  snapshot fixture cross-checked against live mock")
    return 0


if __name__ == "__main__":
    sys.exit(main())
