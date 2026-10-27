#!/usr/bin/env python3
"""Bounded end-to-end LOCAL_MOCK load test; Python standard library only."""
import argparse
import concurrent.futures
import json
import os
import platform
import statistics
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

TERMINAL = {"COMPLETED", "PARTIAL_COMPLETED", "REJECTED", "FAILED", "CANCELLED", "EXPIRED"}


def request(base, path, tenant, body=None, key=None, method=None, timeout=15):
    headers = {"X-Tenant-ID": tenant}
    data = None
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode()
        headers["Content-Type"] = "application/json"
    if key:
        headers["Idempotency-Key"] = key
    req = urllib.request.Request(base.rstrip("/") + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read() or b"{}")


def percentile(values, percent):
    if not values:
        return None
    values = sorted(values)
    position = (len(values) - 1) * percent / 100
    low = int(position)
    high = min(low + 1, len(values) - 1)
    return round(values[low] + (values[high] - values[low]) * (position - low), 3)


def distribution(values):
    return {"count": len(values), "p50": percentile(values, 50), "p95": percentile(values, 95),
            "p99": percentile(values, 99), "mean": round(statistics.mean(values), 3) if values else None}


def run_one(index, args, run_id):
    start = time.monotonic()
    result = {"index": index, "accepted": False, "terminal": False, "status": "CLIENT_ERROR"}
    body = {"text": f"半马跑步训练：如何安排配速和恢复？测试编号 {run_id}-{index}",
            "attachment_ids": [], "max_candidates": args.max_candidates, "deadline_seconds": max(10, int(args.timeout)), "fault": ""}
    key = f"load-{run_id}-{index}"
    try:
        status, created = request(args.base_url, "/api/posts", args.tenant, body, key)
        result.update(accept_http_status=status, accept_latency_ms=round((time.monotonic() - start) * 1000, 3))
        if status != 202:
            result.update(status="ADMISSION_FAILED", error=created)
            return result
        result.update(accepted=True, post_id=created["post_id"], job_id=created["job_id"], accepted_at=time.monotonic())
        if args.duplicate_every and index % args.duplicate_every == 0:
            duplicate_status, duplicate = request(args.base_url, "/api/posts", args.tenant, body, key)
            result["idempotency_verified"] = duplicate_status == 202 and all(duplicate.get(k) == created[k] for k in ("job_id", "post_id"))
        deadline = start + args.timeout
        while time.monotonic() < deadline:
            code, job = request(args.base_url, "/api/jobs/" + created["job_id"], args.tenant, timeout=min(15, max(0.1, deadline-time.monotonic())))
            if code != 200:
                result.update(status="POLL_ERROR", error=job)
                return result
            if job["status"] in TERMINAL:
                result.update(terminal=True, status=job["status"], e2e_latency_ms=round((time.monotonic()-start)*1000, 3), completed_at=time.monotonic(), candidate_count=len(job.get("candidates") or []), operation_count=len(job.get("operations") or []), operation_statuses=[o.get("status") for o in (job.get("operations") or [])])
                code, post = request(args.base_url, "/api/posts/" + created["post_id"], args.tenant)
                if code != 200:
                    result["verification_error"] = {"http": code, "body": post}
                else:
                    interactions = post.get("interactions") or []
                    identities = [(i.get("avatar_id"), i.get("kind", i.get("action_type", i.get("type", i.get("action"))))) for i in interactions]
                    result.update(interaction_count=len(identities), unique_actions_verified=len(identities) == len(set(identities)))
                    result["action_identity_complete"] = all(all(part is not None for part in key) for key in identities)
                return result
            if job["status"] == "NEEDS_RECONCILIATION":
                result.update(status=job["status"], error="Unexpected unresolved provider outcome in normal-load profile")
                return result
            time.sleep(min(args.poll_interval, max(0, deadline-time.monotonic())))
        result.update(status="CLIENT_TIMEOUT", error="Job did not reach a terminal state before the client deadline")
    except Exception as error:
        result["error"] = str(error)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--tenant", default="loadtest")
    parser.add_argument("--requests", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=8, help="Maximum in-flight complete post lifecycles, including polling")
    parser.add_argument("--duplicate-every", type=int, default=5, help="Repeat identical create on every Nth post, 0 disables")
    parser.add_argument("--max-candidates", type=int, default=4)
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--poll-interval", type=float, default=.2)
    parser.add_argument("--outbox-drain-timeout", type=float, default=30, help="Additional wait for tenant event outbox; separate from job completion")
    parser.add_argument("--output", default="data/loadtest-report.json")
    args = parser.parse_args()
    if min(args.requests, args.concurrency, args.timeout, args.poll_interval, args.max_candidates) <= 0 or args.duplicate_every < 0 or args.max_candidates > 5 or args.timeout > 600 or args.outbox_drain_timeout < 0:
        parser.error("Counts, timeout and poll interval must be positive; duplicate-every >= 0; max-candidates <= 5; timeout <= 600")
    run_id = uuid.uuid4().hex[:12]
    _, health = request(args.base_url, "/api/health", args.tenant)
    _, before = request(args.base_url, "/api/stats", args.tenant)
    start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        results = list(pool.map(lambda i: run_one(i, args, run_id), range(args.requests)))
    elapsed = time.monotonic() - start
    _, after = request(args.base_url, "/api/stats", args.tenant)
    event_start = time.monotonic()
    event_stats = after
    outbox_supported = "outbox_pending" in after
    while outbox_supported and event_stats.get("outbox_pending", 0) > 0 and time.monotonic()-event_start < args.outbox_drain_timeout:
        time.sleep(args.poll_interval)
        _, event_stats = request(args.base_url, "/api/stats", args.tenant)
    event_wait = time.monotonic()-event_start
    accepted = [r for r in results if r["accepted"]]
    terminal = [r for r in results if r["terminal"]]
    success = [r for r in results if r["status"] == "COMPLETED"]
    failures = [r for r in results if r["status"] != "COMPLETED"]
    status_counts = {}
    for r in results:
        status_counts[r["status"]] = status_counts.get(r["status"], 0) + 1
    last_accept = max((r["accepted_at"] for r in accepted), default=start)
    invariant_failures = [r["index"] for r in results if r.get("idempotency_verified") is False or r.get("unique_actions_verified") is False or r.get("action_identity_complete") is False or "verification_error" in r or (r["status"] == "COMPLETED" and r.get("candidate_count", 0) == 0)]
    report = {"schema_version": "loadtest.v1", "run_id": run_id, "timestamp_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
              "scope": "LOCAL_MOCK closed-loop, bounded in-flight post lifecycles. Does not measure real AI provider throughput; polling/verification contribute load. Terminal latency excludes timed-out tasks; always read timeout counts.",
              "environment": {"platform": platform.platform(), "python": platform.python_version(), "cpu_count": os.cpu_count(), "server_health": health},
              "workload": {"modality": "text", "topic": "running and marathon", "fault": "none", "model": "deterministic_mock", "concurrency_semantics": "bounded whole post lifecycles, not HTTP-only concurrency"},
              "parameters": vars(args), "elapsed_seconds": round(elapsed, 3), "accepted": len(accepted), "terminal": len(terminal), "completed_successfully": len(success),
              "status_counts": status_counts, "failure_rate_all_submissions": len(failures)/args.requests,
              "admission_failure_rate": (args.requests-len(accepted))/args.requests,
              "accepted_without_terminal": len(accepted)-len(terminal),
              "accept_latency_ms": distribution([r["accept_latency_ms"] for r in accepted]),
              "e2e_terminal_latency_ms": distribution([r["e2e_latency_ms"] for r in terminal]),
              "e2e_success_latency_ms": distribution([r["e2e_latency_ms"] for r in success]),
              "observed_accepts_per_second_until_last_accept": round(len(accepted)/max(last_accept-start, .001), 3),
              "terminal_throughput_per_second": round(len(terminal)/elapsed, 3),
              "successful_completion_throughput_per_second": round(len(success)/elapsed, 3),
              "drain_after_last_accept_seconds": round(max(0, start+elapsed-last_accept), 3),
              "drain_definition": "Job terminal-state drain only; does not imply event/outbox drain.",
              "outbox": {"scope": "tenant-wide, including preexisting events", "supported": outbox_supported,
                         "pending_at_job_completion": after.get("outbox_pending"), "pending_after_wait": event_stats.get("outbox_pending"),
                         "wait_seconds_after_job_completion": round(event_wait, 3),
                         "drained": outbox_supported and event_stats.get("outbox_pending") == 0},
              "candidate_total": sum(r.get("candidate_count", 0) for r in results), "operation_total": sum(r.get("operation_count", 0) for r in results), "zero_candidate_completed": sum(r["status"] == "COMPLETED" and r.get("candidate_count", 0) == 0 for r in results),
              "idempotency_checks": sum("idempotency_verified" in r for r in results), "invariant_failure_indices": invariant_failures,
              "stats_before": before, "stats_after": after, "stats_after_outbox_wait": event_stats, "results": results}
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n", encoding="utf-8")
    print(json.dumps({k: report[k] for k in ("run_id", "accepted", "terminal", "status_counts", "failure_rate_all_submissions", "accept_latency_ms", "e2e_terminal_latency_ms", "successful_completion_throughput_per_second", "invariant_failure_indices")}, ensure_ascii=False, indent=2))
    print(f"Report: {output.resolve()}")
    return 1 if failures or invariant_failures or (outbox_supported and event_stats.get("outbox_pending", 0) > 0) else 0


if __name__ == "__main__":
    raise SystemExit(main())
