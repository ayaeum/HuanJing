#!/usr/bin/env python3
"""Shared HTTP-only E2E harness. No SQLite reads or writes.

Each Server owns a temporary DB, upload directory and ephemeral localhost port.
Use Server.crash()/restart() to test process death with the same durable state.
"""
import atexit
import datetime
import hashlib
import json
import os
import pathlib
import socket
import subprocess
import sys
import tempfile
import time
import traceback
import urllib.error
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]
TERMINAL = {"COMPLETED", "PARTIAL_COMPLETED", "REJECTED", "FAILED", "CANCELLED", "EXPIRED", "SUPERSEDED", "NEEDS_RECONCILIATION"}
_BINARY = None
_BUILD_DIR = None


def build_binary():
    """Build once per Python process, or use explicitly supplied E2E_BINARY."""
    global _BINARY, _BUILD_DIR
    if _BINARY is not None:
        return _BINARY
    configured = os.environ.get("E2E_BINARY")
    if configured:
        path = pathlib.Path(configured).expanduser().resolve()
        if not path.is_file():
            raise RuntimeError("E2E_BINARY does not exist: " + str(path))
        _BINARY = path
        return path
    _BUILD_DIR = tempfile.TemporaryDirectory(prefix="huanjing-e2e-build-")
    atexit.register(_BUILD_DIR.cleanup)
    path = pathlib.Path(_BUILD_DIR.name) / "huanjing"
    result = subprocess.run(["go", "build", "-o", str(path), "./cmd/server"], cwd=ROOT, capture_output=True, text=True, timeout=180)
    if result.returncode:
        raise RuntimeError("Go build failed:\n" + result.stdout + result.stderr)
    _BINARY = path
    return path


class Server:
    def __init__(self, binary=None, workers=4, seed_tenants="demo,other"):
        self.binary = pathlib.Path(binary) if binary else build_binary()
        self.workers = workers
        self.seed_tenants = seed_tenants
        self._tmp = tempfile.TemporaryDirectory(prefix="huanjing-e2e-case-")
        self.directory = pathlib.Path(self._tmp.name)
        self.db_path = self.directory / "system.db"
        self.log_path = self.directory / "server.log"
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            self.port = sock.getsockname()[1]
        self.base = "http://127.0.0.1:" + str(self.port)
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.proc = None
        self.log = None

    def __enter__(self):
        try:
            self.start()
            return self
        except BaseException:
            self.close()
            raise

    def __exit__(self, exc_type, exc, tb):
        if exc is not None:
            tail = self.log_path.read_text(errors="replace")[-3000:] if self.log_path.exists() else ""
            exc.args = (*exc.args, "server log tail: " + tail)
        self.close()
        return False

    def start(self):
        if self.proc is not None and self.proc.poll() is None:
            raise RuntimeError("server is already running")
        if self.log is None:
            self.log = self.log_path.open("a", encoding="utf-8")
        env = dict(os.environ, ADDR="127.0.0.1:" + str(self.port), DB_PATH=str(self.db_path), WEB_DIR=str(ROOT / "web"), WORKERS=str(self.workers), SEED_TENANTS=self.seed_tenants)
        self.proc = subprocess.Popen([str(self.binary)], cwd=ROOT, env=env, stdout=self.log, stderr=self.log)
        end = time.monotonic() + 15
        while time.monotonic() < end:
            if self.proc.poll() is not None:
                raise RuntimeError("server exited during startup:\n" + self.log_path.read_text(errors="replace"))
            try:
                status, body = self.request("GET", "/api/health")
                if status == 200 and body.get("status") == "ok":
                    return
            except (OSError, ValueError):
                pass
            time.sleep(0.03)
        raise AssertionError("server health startup timeout")

    def crash(self):
        if self.proc is not None and self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(timeout=5)

    def stop(self):
        if self.proc is not None and self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.crash()

    def restart(self):
        if self.proc is not None and self.proc.poll() is None:
            self.stop()
        self.start()

    def close(self):
        self.stop()
        if self.log:
            self.log.close()
            self.log = None
        self._tmp.cleanup()

    def request(self, method, path, body=None, key=None, tenant="demo", headers=None, raw=None):
        hdr = {"X-Tenant-ID": tenant}
        if body is not None and raw is not None:
            raise ValueError("use body or raw, not both")
        payload = raw
        if body is not None:
            payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
            hdr["Content-Type"] = "application/json"
        if key:
            hdr["Idempotency-Key"] = key
        hdr.update(headers or {})
        req = urllib.request.Request(self.base + path, data=payload, headers=hdr, method=method)
        try:
            response = self.opener.open(req, timeout=12)
        except urllib.error.HTTPError as err:
            response = err
        with response:
            data = response.read()
            try:
                parsed = json.loads(data)
            except (ValueError, UnicodeDecodeError):
                parsed = data.decode("utf-8", errors="replace")
            return response.status, parsed

    raw = request

    def expect(self, method, path, body=None, key=None, tenant="demo", headers=None, raw=None, status=200):
        code, value = self.request(method, path, body=body, key=key, tenant=tenant, headers=headers, raw=raw)
        allowed = (status,) if isinstance(status, int) else tuple(status)
        assert code in allowed, {"method": method, "path": path, "expected_status": allowed, "actual_status": code, "response": value}
        return value

    def create(self, text="徒步旅行自然摄影", max_candidates=1, fault="", deadline_seconds=120, key=None, tenant="demo", **extra):
        body = dict(text=text, max_candidates=max_candidates, fault=fault, deadline_seconds=deadline_seconds)
        body.update(extra)
        return self.expect("POST", "/api/posts", body, key=key or uuid.uuid4().hex, tenant=tenant, status=202)

    def job(self, job_id, tenant="demo"):
        return self.expect("GET", "/api/jobs/" + job_id, tenant=tenant)

    def until(self, predicate, timeout=25, description="condition", interval=0.025):
        end = time.monotonic() + timeout
        last = None
        while time.monotonic() < end:
            last = predicate()
            if last:
                return last
            time.sleep(interval)
        raise AssertionError({"timeout_seconds": timeout, "waiting_for": description, "last": last})

    def wait(self, job_id, statuses=None, stage=None, timeout=25, tenant="demo"):
        last = None
        wanted = set(statuses) if statuses else TERMINAL
        if isinstance(statuses, str):
            wanted = {statuses}
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            last = self.job(job_id, tenant)
            if stage is not None:
                if last["stage"] == stage and (statuses is None or last["status"] in wanted):
                    return last
            elif last["status"] in wanted:
                return last
            time.sleep(0.025)
        raise AssertionError({"timeout_seconds": timeout, "desired_status": sorted(wanted), "desired_stage": stage, "job": last})

    def operation_running(self, job_id, name, timeout=25):
        def check():
            j = self.job(job_id)
            found = [o for o in j.get("operations", []) if o["name"] == name and o["status"] == "RUNNING"]
            return j if found else None
        return self.until(check, timeout=timeout, description=name + " operation RUNNING")


class Report:
    def __init__(self, suite, path=None):
        self.suite = suite
        self.path = pathlib.Path(path) if path else ROOT / "reports" / (suite + ".json")
        self.started = datetime.datetime.now(datetime.timezone.utc).isoformat()
        self.cases = []

    def run(self, name, expected, fn):
        start = time.monotonic()
        row = {"name": name, "expected": expected}
        try:
            actual = fn()
            row.update(status="PASS", actual=actual if actual is not None else "All HTTP assertions passed", failure=None)
        except Exception as exc:
            row.update(status="FAIL", actual="Case did not satisfy its assertions", failure={"type": type(exc).__name__, "message": str(exc), "traceback": traceback.format_exc()})
        row["elapsed_seconds"] = round(time.monotonic() - start, 4)
        self.cases.append(row)
        print("[%s] %s (%.3fs)" % (row["status"], name, row["elapsed_seconds"]), flush=True)
        if row["status"] == "FAIL":
            print(row["failure"]["message"], flush=True)
        return row["status"] == "PASS"

    def finish(self):
        failed = sum(c["status"] == "FAIL" for c in self.cases)
        binary = build_binary()
        doc = {"suite": self.suite, "started_at_utc": self.started, "finished_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "scope": "HTTP-only black-box; isolated SQLite state and random localhost port per case; deterministic MOCK providers", "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(), "cases_total": len(self.cases), "passed": len(self.cases) - failed, "failed": failed, "elapsed_seconds": round(sum(c["elapsed_seconds"] for c in self.cases), 4), "cases": self.cases}
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.write_text(json.dumps(doc, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print("Report: %s — %d/%d passed" % (self.path, doc["passed"], doc["cases_total"]), flush=True)
        return 1 if failed else 0
