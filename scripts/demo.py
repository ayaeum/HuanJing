#!/usr/bin/env python3
"""Upload real test media and exercise normal/failure/reconciliation mock paths."""
import argparse
import json
import mimetypes
import time
import urllib.request
import uuid
from pathlib import Path
from loadtest import request, TERMINAL


def upload(base, tenant, path, caption):
    boundary = "huanjing-" + uuid.uuid4().hex
    body = (f'--{boundary}\r\nContent-Disposition: form-data; name="caption"\r\n\r\n{caption}\r\n'
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{path.name}"\r\n'
            f'Content-Type: {mimetypes.guess_type(path.name)[0] or "application/octet-stream"}\r\n\r\n').encode() + path.read_bytes() + f"\r\n--{boundary}--\r\n".encode()
    req = urllib.request.Request(base.rstrip("/")+"/api/uploads", data=body, headers={"X-Tenant-ID":tenant,"Content-Type":"multipart/form-data; boundary="+boundary})
    with urllib.request.urlopen(req, timeout=20) as response:
        return json.load(response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--tenant", default="demo")
    parser.add_argument("--output", default="data/demo-report.json")
    parser.add_argument("--timeout", type=float, default=120)
    args = parser.parse_args()
    if args.timeout <= 0 or args.timeout > 600:
        parser.error("timeout must be positive and <= 600 seconds")
    fixture_dir = Path(__file__).resolve().parents[1]/"fixtures"
    run_id = uuid.uuid4().hex[:10]
    scenarios = [("text",None,"", ""), ("image","running.png","模拟跑步配速截图：半马训练经验", ""),
                 ("audio","training.wav","模拟语音转写：跑步训练如何安排恢复；实际文件为正弦音", ""),
                 ("video","outdoor.mp4","模拟户外旅行视频语义；实际文件为动态测试图", ""),
                 ("retry",None,"","retry_once"), ("unknown",None,"","unknown_once"),
                 ("comment_rejected",None,"","comment_reject"), ("dependency_failure",None,"","always_fail")]
    records = []
    for name, filename, caption, fault in scenarios:
        attachment_ids = []
        if filename:
            attachment_ids.append(upload(args.base_url,args.tenant,fixture_dir/filename,caption)["id"])
        body = {"text":f"第一次参加半程马拉松跑步，想请教配速训练与恢复。演示 {name} / {run_id}", "attachment_ids":attachment_ids, "max_candidates":4,"deadline_seconds":max(10,int(args.timeout)),"fault":fault}
        key = f"demo-{run_id}-{name}"
        status, created = request(args.base_url,"/api/posts",args.tenant,body,key)
        if status != 202:
            records.append({"scenario":name,"error":created,"http":status})
            continue
        dup_status, duplicate = request(args.base_url,"/api/posts",args.tenant,body,key)
        records.append({"scenario":name,"fault":fault,"job_id":created["job_id"],"post_id":created["post_id"],"idempotency_verified":dup_status==202 and duplicate.get("job_id")==created["job_id"] and duplicate.get("post_id")==created["post_id"]})
    end = time.monotonic()+args.timeout
    pending = [r for r in records if "job_id" in r]
    while pending and time.monotonic()<end:
        for record in pending[:]:
            code, job = request(args.base_url,"/api/jobs/"+record["job_id"],args.tenant)
            if code != 200:
                record["error"] = job
                continue
            record["status"] = job["status"]
            if job["status"] == "NEEDS_RECONCILIATION" and not record.get("reconciled"):
                code, reply = request(args.base_url,"/api/jobs/"+record["job_id"]+"/reconcile",args.tenant,{},method="POST")
                record["reconciled"] = code in (200,202)
                record["reconcile_response"] = reply
            if job["status"] in TERMINAL:
                record["job"] = job
                _, record["post"] = request(args.base_url,"/api/posts/"+record["post_id"],args.tenant)
                pending.remove(record)
                print(f'{record["scenario"]}: {record["status"]}', flush=True)
        if pending:
            time.sleep(.25)
    for record in pending:
        record["client_timeout"] = True
    expected = {"dependency_failure": {"FAILED", "EXPIRED"}, "comment_rejected": {"PARTIAL_COMPLETED"}}
    for record in records:
        record["expected_status_verified"] = record.get("status") in expected.get(record["scenario"], {"COMPLETED"})
        if record.get("status") in {"COMPLETED", "PARTIAL_COMPLETED"}:
            record["nonempty_candidates_verified"] = bool(record.get("job", {}).get("candidates"))
            interactions = record.get("post", {}).get("interactions") or []
            identities = [(i.get("avatar_id"), i.get("kind")) for i in interactions]
            record["unique_actions_verified"] = len(identities) == len(set(identities))
        if record["scenario"] == "unknown":
            record["unknown_recovery_verified"] = record.get("reconciled") is True
    report = {"schema_version":"demo.v1","mode":"LOCAL_MOCK","run_id":run_id,"media_notice":"Image/video are test patterns, audio is a sine tone. Captions supply simulated semantic evidence, not real OCR/ASR/vision.","parameters":vars(args),"scenarios":records}
    output=Path(args.output);output.parent.mkdir(parents=True,exist_ok=True)
    output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+"\n",encoding="utf-8")
    print(f"Report: {output.resolve()}")
    checks_failed = any("error" in r or not r.get("idempotency_verified") or
                        not r.get("expected_status_verified") or
                        any(value is False for key, value in r.items() if key.endswith("_verified")) for r in records)
    return 1 if pending or checks_failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
