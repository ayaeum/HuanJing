# Local API contract (v1)

Base: http://127.0.0.1:8080. `X-Tenant-ID` defaults to `demo`; tenant header is demo isolation, not production authentication. JSON requests/responses; errors `{error:"..."}`. All timestamps UNIX milliseconds. Persistent SQLite database defaults `data/system.db`. Models and embeddings explicitly deterministic MOCK; uploaded media are not actually perceived, `caption` supplies simulated semantics.

- `GET /api/health`: `{status,mode}`
- `GET /api/avatars`: `{avatars:[{id,name,persona,interests,active,public_memories:[]}]}`; private memories never returned.
- `PATCH /api/avatars/{id}`: `{active:false}` revokes avatar, epoch increments.
- `POST /api/uploads`: multipart `file` and `caption`; up to 20 MiB; image/audio/video accepted. Returns `{id,name,mime,size,caption,url}`. `/api/uploads/{id}` authenticated by demo tenant header.
- `POST /api/posts`: header `Idempotency-Key` required. Body `{text:"...",attachment_ids:[],max_candidates:4, fault:"",deadline_seconds:120}`. Fault options `retry_once`, `always_fail`, `unknown_once`, `comment_reject`, `slow`. Returns 202 `{post_id,job_id,status:"ACCEPTED"}`; same key+input returns same IDs, changed input 409.
- `GET /api/posts`: `{posts:[{id,text,version,status,created_at,job_id,deleted,attachments:[],interactions:[]}]}` newest first.
- `GET /api/posts/{id}`: same post object; deleted returns 410.
- `DELETE /api/posts/{id}`: `{status:"DELETED"}`; cancels job and hides interactions atomically.
- `GET /api/jobs`: `{jobs:[job...]}`.
- `GET /api/jobs/{id}`: `{id,post_id,status,stage,attempts,error,created_at,updated_at,deadline,epoch,next_run,stages:[],candidates:[],operations:[],events:[]}`. Stages have name/status/output; candidates have avatar_id,name,score,evidence,actions,status,reason,comment. Operations expose operation_key,status,attempt,cost,error (no private prompts). Stages: MODERATE, UNDERSTAND, MATCH, DECIDE, PUBLISH, DONE. Terminal states COMPLETED/PARTIAL_COMPLETED/REJECTED/FAILED/CANCELLED/EXPIRED; retry WAITING_DEPENDENCY; unknown NEEDS_RECONCILIATION.
- `POST /api/jobs/{id}/retry`: retry FAILED/EXPIRED job with new epoch/deadline, reuses completed stages; does not revive deleted posts.
- `POST /api/jobs/{id}/reconcile`: reconcile simulated unknown provider receipt; returns `{status:"ACCEPTED"}`. An unknown simulation has a persisted provider receipt; re-running never double charges that request.
- `GET /api/stats`: `{posts,jobs,interactions,queue_depth,statuses:{...},budget:{limit,spent,reserved},mode:"LOCAL_MOCK"}`.
- `POST /api/budget`: `{limit:1000}` local demonstration administration, limit must >= spent+reserved.
- `GET /metrics`: Prometheus text counters/gauges, no unbounded labels.

LIKE and COMMENT independently unique per post/avatar/action. Private memory can influence mock decision only; comment formatter receives explicit public context. Seeds in demo include travel, fitness, cooking, technology, art personas. Jobs survive process death with a 3-second renewable SQLite lease; defaults 4 workers. See README for production limitations.

## Implemented additions

- `PATCH /api/posts/{id}`: `{expected_version:1,text:"new text",attachment_ids:[]}` returns a new `job_id` and `version`. Old active jobs are SUPERSEDED; final frozen LIKE/COMMENT plan survives edits, actions remain individually unique.
- `GET /api/memories`: metadata and public text only; private content is redacted. `DELETE /api/memories/{id}` revokes a source immediately. Final action transactions validate referenced memories and source post tombstones.
- Candidate `action_states` maps `LIKE`/`COMMENT` to PENDING/SUCCEEDED/UNKNOWN/REJECTED/STALE.
- `/api/stats` includes `outbox_pending` and `outbox_done` (delivered or source-revoked). Projector processes up to 32 events per 100 ms tick, one short idempotent transaction each.
- `SEED_TENANTS=demo,loadtest` seeds an explicitly configured list. No automatic seeding of arbitrary request headers.
- Reconciliation also settles UNKNOWN charges after a job is CANCELLED or SUPERSEDED; it preserves that terminal state and never revives publication.
- All budget values are mock integer cost units (one unit per independent simulated provider attempt), not currency or real token billing.

### Deterministic mock action examples
For relevant text such as `徒步旅行摄影`, the default mock plan is LIKE + COMMENT. Adding `只评论` selects COMMENT only, `只点赞` selects LIKE only, and `不需要互动` selects an empty plan. These are explicit local mock-model demonstration rules; they never bypass authorization, budget, moderation, or action uniqueness. Real model selection is not implemented.
