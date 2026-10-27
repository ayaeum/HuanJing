#!/usr/bin/env python3
"""Lifecycle E2E: isolated real HTTP server per case; no database access.

Run: python3 scripts/e2e_lifecycle.py [--report reports/e2e-lifecycle.json]
Optionally E2E_BINARY=/absolute/path/to/server reuses an already built binary.
"""
import argparse
import concurrent.futures
import json
import sys

from e2e_support import ROOT, Report, Server, build_binary

TEXT = "徒步旅行自然摄影"


def actions(server, post_id, avatar="travel"):
    post = server.expect("GET", "/api/posts/" + post_id)
    found = [i for i in post["interactions"] if i["avatar_id"] == avatar]
    keys = [(i["avatar_id"], i["kind"]) for i in post["interactions"]]
    assert len(keys) == len(set(keys)), "duplicate independently idempotent action"
    return found


def assert_plan(server, post_id, expected):
    found = actions(server, post_id)
    kinds = sorted(i["kind"] for i in found)
    assert kinds == sorted(expected), {"expected": expected, "actual": kinds}
    return kinds


def settled(server, job_id, expected="COMPLETED"):
    job = server.wait(job_id)
    assert job["status"] == expected, job
    return job


def case_plan(binary, suffix, expected):
    with Server(binary) as s:
        p = s.create(TEXT + suffix)
        job = settled(s, p["job_id"])
        actual = assert_plan(s, p["post_id"], expected)
        candidate = next(c for c in job["candidates"] if c["avatar_id"] == "travel")
        assert sorted(candidate["actions"]) == sorted(expected), candidate
        if not expected:
            assert candidate["status"] == "IGNORED", candidate
        else:
            assert all(candidate["action_states"][kind] == "SUCCEEDED" for kind in expected), candidate
        assert "PRIVATE_CANARY" not in json.dumps(job), "private context leaked in job read model"
        return {"job_status": job["status"], "planned_actions": candidate["actions"], "published_actions": actual}


def case_output_rejection(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="comment_reject")
        job = settled(s, p["job_id"], "PARTIAL_COMPLETED")
        assert_plan(s, p["post_id"], ["LIKE"])
        c = job["candidates"][0]
        assert c["action_states"] == {"COMMENT": "REJECTED", "LIKE": "SUCCEEDED"}, c
        return {"status": job["status"], "actions": c["action_states"]}


def case_transient_retry(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="retry_once")
        job = settled(s, p["job_id"])
        ops = [o for o in job["operations"] if o["name"] == "understand"]
        assert [(o["attempt"], o["status"]) for o in ops] == [(1, "FAILED"), (2, "SUCCEEDED")], ops
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["spent"] == 4 and budget["reserved"] == 0, budget
        return {"attempts": [(o["attempt"], o["status"]) for o in ops], "budget": budget}


def case_permanent_failure(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="always_fail")
        job = settled(s, p["job_id"], "FAILED")
        ops = [o for o in job["operations"] if o["name"] == "understand"]
        assert len(ops) == 3 and all(o["status"] == "FAILED" for o in ops), ops
        assert_plan(s, p["post_id"], [])
        assert job["stage"] == "UNDERSTAND" and "503" in job["error"], job
        return {"status": job["status"], "provider_attempts": len(ops), "published_actions": []}


def case_budget_zero_then_recovery(binary):
    with Server(binary) as s:
        s.expect("POST", "/api/budget", {"limit": 0})
        p = s.create(TEXT)
        failed = settled(s, p["job_id"], "FAILED")
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget == {"limit": 0, "spent": 0, "reserved": 0}, budget
        assert failed["operations"] == [], failed
        s.expect("POST", "/api/budget", {"limit": 3})
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/retry", status=202)
        done = settled(s, p["job_id"])
        assert done["epoch"] > failed["epoch"], (failed, done)
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        return {"before": budget, "after": s.expect("GET", "/api/stats")["budget"], "recovered_status": done["status"]}


def case_exact_budget(binary):
    with Server(binary) as s:
        s.expect("POST", "/api/budget", {"limit": 3})
        p = s.create(TEXT)
        settled(s, p["job_id"])
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget == {"limit": 3, "spent": 3, "reserved": 0}, budget
        s.expect("POST", "/api/budget", {"limit": 2}, status=409)
        p2 = s.create(TEXT)
        settled(s, p2["job_id"], "FAILED")
        assert_plan(s, p2["post_id"], [])
        assert s.expect("GET", "/api/stats")["budget"] == budget
        return {"budget": budget, "second_job": "FAILED", "lower_limit_http": 409}


def case_partial_budget_topup(binary):
    with Server(binary) as s:
        s.expect("POST", "/api/budget", {"limit": 2})
        p = s.create(TEXT)
        failed = settled(s, p["job_id"], "FAILED")
        before = actions(s, p["post_id"])
        assert [x["kind"] for x in before] == ["LIKE"], before
        assert s.expect("GET", "/api/stats")["budget"] == {"limit": 2, "spent": 2, "reserved": 0}
        s.expect("POST", "/api/budget", {"limit": 3})
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/retry", status=202)
        settled(s, p["job_id"])
        after = actions(s, p["post_id"])
        assert {x["kind"] for x in after} == {"LIKE", "COMMENT"}, after
        assert next(x["id"] for x in after if x["kind"] == "LIKE") == before[0]["id"], "retry replaced an already committed LIKE"
        return {"failed_stage": failed["stage"], "like_receipt_preserved": True, "final_actions": sorted(x["kind"] for x in after)}


def case_unknown_reconcile(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="unknown_once")
        unknown = settled(s, p["job_id"], "NEEDS_RECONCILIATION")
        assert_plan(s, p["post_id"], ["LIKE"])
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["spent"] == 2 and budget["reserved"] == 1, budget
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/retry", status=409)
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=202)
        done = settled(s, p["job_id"])
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        assert len([o for o in done["operations"] if o["name"] == "comment"]) == 1, done
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=409)
        return {"unknown_actions": unknown["candidates"][0]["action_states"], "final_actions": done["candidates"][0]["action_states"], "budget": s.expect("GET", "/api/stats")["budget"]}


def case_unknown_delete(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="unknown_once")
        settled(s, p["job_id"], "NEEDS_RECONCILIATION")
        s.expect("DELETE", "/api/posts/" + p["post_id"])
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=202)
        cancelled = settled(s, p["job_id"], "CANCELLED")
        s.expect("GET", "/api/posts/" + p["post_id"], status=410)
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["reserved"] == 0 and budget["spent"] == 3, budget
        assert cancelled.get("candidates") == [], "deleted content leaked through job candidate read model"
        return {"status": cancelled["status"], "post_http": 410, "budget": budget}


def case_unknown_edit(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="unknown_once")
        settled(s, p["job_id"], "NEEDS_RECONCILIATION")
        edited = s.expect("PATCH", "/api/posts/" + p["post_id"], {"expected_version": 1, "text": TEXT + "，更新后的内容"}, status=202)
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=202)
        old = settled(s, p["job_id"], "SUPERSEDED")
        new = settled(s, edited["job_id"])
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["reserved"] == 0, budget
        return {"old_status": old["status"], "new_status": new["status"], "travel_actions": ["LIKE", "COMMENT"], "reserved": budget["reserved"]}


def case_restart_running_understand(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        before = s.operation_running(p["job_id"], "understand")
        s.crash()
        s.restart()
        job = settled(s, p["job_id"])
        assert job["epoch"] > before["epoch"], (before, job)
        understand = [o for o in job["operations"] if o["name"] == "understand"]
        assert len(understand) == 1 and understand[0]["status"] == "SUCCEEDED", understand
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["spent"] == 3 and budget["reserved"] == 0, budget
        return {"epoch_before": before["epoch"], "epoch_after": job["epoch"], "understand_provider_attempts": 1, "budget": budget}


def case_restart_after_like(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "comment")
        before = actions(s, p["post_id"])
        assert [i["kind"] for i in before] == ["LIKE"], before
        s.crash()
        s.restart()
        job = settled(s, p["job_id"])
        after = actions(s, p["post_id"])
        assert len(after) == 2 and next(i["id"] for i in after if i["kind"] == "LIKE") == before[0]["id"], after
        assert len([o for o in job["operations"] if o["name"] == "comment"]) == 1, job
        return {"preserved_like_receipt": True, "comment_attempts": 1, "actions": sorted(i["kind"] for i in after)}


def case_restart_unknown(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="unknown_once")
        settled(s, p["job_id"], "NEEDS_RECONCILIATION")
        before = s.expect("GET", "/api/stats")["budget"]
        s.crash()
        s.restart()
        job = settled(s, p["job_id"], "NEEDS_RECONCILIATION")
        assert s.expect("GET", "/api/stats")["budget"] == before
        assert_plan(s, p["post_id"], ["LIKE"])
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=202)
        settled(s, p["job_id"])
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        return {"status_after_restart_before_reconcile": job["status"], "reserved_preserved": before["reserved"], "final_status": "COMPLETED"}


def case_running_cancel_accounting(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "understand")
        s.crash()
        s.restart()
        s.expect("DELETE", "/api/posts/" + p["post_id"])
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/reconcile", status=202)
        settled(s, p["job_id"], "CANCELLED")
        budget = s.expect("GET", "/api/stats")["budget"]
        assert budget["reserved"] == 0 and budget["spent"] == 1, budget
        assert s.expect("GET", "/api/stats")["interactions"] == 0
        return {"status": "CANCELLED", "durable_running_receipt_settled": True, "budget": budget}


def case_parallel_version_edit(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "understand")
        def edit(index):
            return s.request("PATCH", "/api/posts/" + p["post_id"], {"expected_version": 1, "text": TEXT + "，修改" + str(index)})
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(edit, [1, 2]))
        assert sorted(code for code, _ in results) == [202, 409], results
        accepted = next(body for code, body in results if code == 202)
        settled(s, accepted["job_id"])
        s.until(lambda: s.expect("GET", "/api/stats")["active_workers"] == 0, description="old in-flight worker returned")
        old = settled(s, p["job_id"], "SUPERSEDED")
        post = s.expect("GET", "/api/posts/" + p["post_id"])
        assert post["version"] == 2 and post["job_id"] == accepted["job_id"], post
        assert_plan(s, p["post_id"], ["LIKE", "COMMENT"])
        return {"concurrent_edit_codes": sorted(code for code, _ in results), "version": post["version"], "old_status_after_late_worker": old["status"]}


def case_frozen_like(binary):
    with Server(binary) as s:
        p = s.create(TEXT + "，只点赞")
        settled(s, p["job_id"])
        old_like = actions(s, p["post_id"])[0]["id"]
        edited = s.expect("PATCH", "/api/posts/" + p["post_id"], {"expected_version": 1, "text": TEXT + "，只评论"}, status=202)
        job = settled(s, edited["job_id"])
        assert_plan(s, p["post_id"], ["LIKE"])
        assert actions(s, p["post_id"])[0]["id"] == old_like
        c = next(c for c in job["candidates"] if c["avatar_id"] == "travel")
        assert c["actions"] == ["LIKE"], c
        return {"new_text_requested_mock": "COMMENT", "frozen_plan": c["actions"], "like_receipt_unchanged": True}


def case_frozen_ignore(binary):
    with Server(binary) as s:
        p = s.create(TEXT + "，不需要互动")
        settled(s, p["job_id"])
        edited = s.expect("PATCH", "/api/posts/" + p["post_id"], {"expected_version": 1, "text": TEXT}, status=202)
        job = settled(s, edited["job_id"])
        assert_plan(s, p["post_id"], [])
        c = next(c for c in job["candidates"] if c["avatar_id"] == "travel")
        assert c["actions"] == [] and c["status"] == "IGNORED", c
        return {"frozen_plan": [], "candidate_status": c["status"]}


def case_delete_inflight(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "understand")
        s.expect("DELETE", "/api/posts/" + p["post_id"])
        s.until(lambda: s.expect("GET", "/api/stats")["active_workers"] == 0, description="cancelled in-flight model returned")
        settled(s, p["job_id"], "CANCELLED")
        s.expect("GET", "/api/posts/" + p["post_id"], status=410)
        assert not s.expect("GET", "/api/posts")["posts"]
        assert s.expect("GET", "/api/stats")["interactions"] == 0
        s.expect("POST", "/api/jobs/" + p["job_id"] + "/retry", status=409)
        return {"status_after_late_response": "CANCELLED", "visible_posts": 0, "interactions": 0, "retry_http": 409}


def case_avatar_revoke(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "decision")
        s.expect("PATCH", "/api/avatars/travel", {"active": False})
        job = settled(s, p["job_id"], "FAILED")
        assert_plan(s, p["post_id"], [])
        c = job["candidates"][0]
        assert c["action_states"] == {"LIKE": "STALE", "COMMENT": "STALE"}, c
        return {"status": job["status"], "action_states": c["action_states"], "interactions": 0}


def case_memory_revoke(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow")
        s.operation_running(p["job_id"], "decision")
        memories = s.expect("GET", "/api/memories")["memories"]
        memory = next(m for m in memories if m["avatar_id"] == "travel" and not m["public"] and not m["source_post"])
        s.expect("DELETE", "/api/memories/" + memory["id"])
        job = settled(s, p["job_id"], "FAILED")
        assert_plan(s, p["post_id"], [])
        assert "PRIVATE_CANARY" not in json.dumps(job)
        return {"revoked_source_type": "private seed memory", "status": job["status"], "action_states": job["candidates"][0]["action_states"]}


def case_source_post_delete(binary):
    with Server(binary) as s:
        source = s.create(TEXT)
        settled(s, source["job_id"])
        s.until(lambda: s.expect("GET", "/api/stats")["outbox_pending"] == 0, description="source memory outbox drained")
        memories = s.expect("GET", "/api/memories")["memories"]
        assert len([m for m in memories if m["source_post"] == source["post_id"]]) == 2, memories
        target = s.create(TEXT, fault="slow")
        s.operation_running(target["job_id"], "decision")
        s.expect("DELETE", "/api/posts/" + source["post_id"])
        job = settled(s, target["job_id"], "FAILED")
        assert_plan(s, target["post_id"], [])
        revoked = [m for m in s.expect("GET", "/api/memories")["memories"] if m["source_post"] == source["post_id"]]
        assert revoked and all(m["revoked"] for m in revoked), revoked
        return {"source_memories_revoked": len(revoked), "target_status": job["status"], "target_actions": []}


def case_deadline(binary):
    with Server(binary) as s:
        p = s.create(TEXT, fault="slow", deadline_seconds=2)
        job = settled(s, p["job_id"], "EXPIRED")
        s.until(lambda: s.expect("GET", "/api/stats")["active_workers"] == 0, description="expired provider attempts returned")
        assert s.job(p["job_id"])["status"] == "EXPIRED", "late worker overwrote deadline outcome"
        assert_plan(s, p["post_id"], [])
        return {"status": job["status"], "interactions": 0, "status_after_late_response": "EXPIRED"}


def case_concurrent_idempotency(binary):
    with Server(binary, workers=8) as s:
        key = "concurrent-identical-publication"
        def publish(_):
            return s.create(TEXT, key=key)
        with concurrent.futures.ThreadPoolExecutor(max_workers=10) as pool:
            replies = list(pool.map(publish, range(10)))
        ids = {r["job_id"] for r in replies}
        assert len(ids) == 1, replies
        settled(s, next(iter(ids)))
        assert_plan(s, replies[0]["post_id"], ["LIKE", "COMMENT"])
        stats = s.expect("GET", "/api/stats")
        assert stats["posts"] == 1 and stats["jobs"] == 1 and stats["interactions"] == 2, stats
        return {"concurrent_requests": 10, "unique_posts": stats["posts"], "unique_jobs": stats["jobs"], "interactions": stats["interactions"]}


def case_budget_persist_restart(binary):
    with Server(binary) as s:
        s.expect("POST", "/api/budget", {"limit": 3})
        p = s.create(TEXT)
        settled(s, p["job_id"])
        before = s.expect("GET", "/api/stats")["budget"]
        s.crash()
        s.restart()
        assert s.expect("GET", "/api/stats")["budget"] == before, "restart reset hard budget"
        another = s.create(TEXT)
        settled(s, another["job_id"], "FAILED")
        assert_plan(s, another["post_id"], [])
        return {"persistent_budget": before, "next_job_after_restart": "FAILED"}


def case_concurrent_hard_budget(binary):
    with Server(binary, workers=8) as s:
        s.expect("POST", "/api/budget", {"limit": 5})
        with concurrent.futures.ThreadPoolExecutor(max_workers=10) as pool:
            posted = list(pool.map(lambda _: s.create(TEXT), range(10)))
        observed = []
        def all_settled():
            stats = s.expect("GET", "/api/stats")
            budget = stats["budget"]
            used = budget["spent"] + budget["reserved"]
            assert used <= 5 and budget["reserved"] >= 0 and budget["spent"] >= 0, budget
            observed.append(used)
            return stats if stats["queue_depth"] == 0 else None
        stats = s.until(all_settled, timeout=30, description="all budget-limited concurrent jobs terminal")
        jobs = [s.job(p["job_id"]) for p in posted]
        assert all(j["status"] in {"COMPLETED", "FAILED", "PARTIAL_COMPLETED"} for j in jobs), jobs
        assert stats["budget"]["reserved"] == 0 and stats["budget"]["spent"] == 5, stats
        return {"concurrent_posts": len(posted), "samples": len(observed), "peak_observed_spent_plus_reserved": max(observed), "limit": 5, "final_budget": stats["budget"], "job_statuses": stats["statuses"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", default=str(ROOT / "reports" / "e2e-lifecycle.json"))
    args = parser.parse_args()
    binary = build_binary()
    report = Report("e2e-lifecycle", args.report)
    plans = [("01-ignore-plan", "不需要互动", []), ("02-like-only-plan", "只点赞", ["LIKE"]), ("03-comment-only-plan", "只评论", ["COMMENT"]), ("04-combined-plan", "", ["LIKE", "COMMENT"])]
    for name, suffix, expected in plans:
        report.run(name, "Exactly the frozen plan %s is published, with independent receipts and no private prompt leakage" % expected, lambda suffix=suffix, expected=expected: case_plan(binary, "，" + suffix if suffix else "", expected))
    cases = [
        ("05-comment-output-rejected", "LIKE survives; COMMENT is REJECTED; job PARTIAL_COMPLETED", case_output_rejection),
        ("06-transient-provider-retry", "One failed and one successful understanding attempt; both actions once; 4 cost units", case_transient_retry),
        ("07-bounded-provider-failure", "Exactly 3 failed provider attempts; FAILED with no fake IGNORE or interactions", case_permanent_failure),
        ("08-zero-budget-topup-recovery", "Zero-cost failure, explicit budget top-up and retry completes without overspend", case_budget_zero_then_recovery),
        ("09-exact-budget-and-lower-limit", "3 units complete one job; reducing below spent returns 409; next job fails without overspend", case_exact_budget),
        ("10-partial-budget-action-retry", "2-unit budget preserves LIKE; 1-unit top-up adds COMMENT and retains LIKE receipt", case_partial_budget_topup),
        ("11-unknown-receipt-reconcile", "UNKNOWN retains reservation and prior LIKE; ordinary retry rejected; reconciliation adds one COMMENT", case_unknown_reconcile),
        ("12-unknown-delete-accounting", "Deleting UNKNOWN job hides post and cancels; receipt accounting settles without republishing", case_unknown_delete),
        ("13-unknown-edit-accounting", "Old UNKNOWN job remains SUPERSEDED after accounting; latest version completes with unique actions", case_unknown_edit),
        ("14-restart-running-understand", "SIGKILL recovers leased stage from one durable provider receipt with no double charge", case_restart_running_understand),
        ("15-restart-after-like", "SIGKILL during COMMENT preserves LIKE receipt and recovers one COMMENT", case_restart_after_like),
        ("16-restart-unknown-state", "Restart preserves UNKNOWN and reserved cost until explicit reconciliation", case_restart_unknown),
        ("17-cancel-orphan-running-receipt", "SIGKILL then cancellation permits cost settlement of orphan RUNNING receipt, with zero interactions", case_running_cancel_accounting),
        ("18-concurrent-edit-version-fence", "Two expected_version=1 edits yield one 202 and one 409; old worker remains SUPERSEDED", case_parallel_version_edit),
        ("19-edit-frozen-like-plan", "New text requesting COMMENT cannot expand a previously frozen LIKE-only plan", case_frozen_like),
        ("20-edit-frozen-ignore-plan", "A committed empty decision remains empty after content edit", case_frozen_ignore),
        ("21-delete-inflight-fence", "Delete during model call prevents late publication, hides post, and rejects retry", case_delete_inflight),
        ("22-avatar-revocation-fence", "Revoking candidate during decision makes both actions STALE and job FAILED", case_avatar_revoke),
        ("23-private-memory-revocation", "Decision using a memory later revoked cannot publish or leak private content", case_memory_revoke),
        ("24-derived-source-deletion", "Deleting a source post revokes derived memory and fences another post's pending actions", case_source_post_delete),
        ("25-business-deadline-fence", "Slow job reaches EXPIRED; late provider result cannot publish or overwrite terminal status", case_deadline),
        ("26-concurrent-idempotent-submit", "10 simultaneous identical submissions create one post/job and exactly two independent actions", case_concurrent_idempotency),
        ("27-budget-survives-restart", "Spent budget survives process death; restart cannot regain consumed units", case_budget_persist_restart),
        ("28-concurrent-hard-budget", "10 posts with 8 workers share 5 units; spent+reserved never exceeds 5 and all jobs settle", case_concurrent_hard_budget),
    ]
    for name, expected, fn in cases:
        report.run(name, expected, lambda fn=fn: fn(binary))
    return report.finish()


if __name__ == "__main__":
    sys.exit(main())
