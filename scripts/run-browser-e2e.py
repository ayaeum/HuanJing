#!/usr/bin/env python3
"""Run the browser E2E suite using the installed ego-browser runtime."""
import argparse
import json
import hashlib
import subprocess
from pathlib import Path

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8084")
parser.add_argument("--space-id", type=int, help="Resume the same TaskSpace after an interrupted run")
parser.add_argument("--server-binary", type=Path, help="Record the tested server binary SHA-256 in the report")
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
build = {"git_head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()}
if args.server_binary:
    build["binary_path"] = str(args.server_binary.resolve())
    build["binary_sha256"] = hashlib.sha256(args.server_binary.read_bytes()).hexdigest()
config = {"root": str(root), "base": args.base_url, "space": args.space_id, "build": build}
source = "globalThis.HJ_E2E_CONFIG = " + json.dumps(config) + ";\n"
source += (root / "scripts/e2e-browser.mjs").read_text(encoding="utf-8")
result = subprocess.run(["ego-browser", "nodejs"], input=source, text=True)
if result.returncode == 0:
    report = json.loads((root / "reports/e2e-browser.json").read_text(encoding="utf-8"))
    if report.get("failed", 1) != 0 or not report.get("finished_at"):
        raise SystemExit(1)
raise SystemExit(result.returncode)
