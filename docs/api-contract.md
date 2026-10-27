# Local API contract (v1)

Base: http://127.0.0.1:8080. `X-Tenant-ID` defaults to `demo`; tenant header is demo isolation, not production authentication. JSON requests/responses; errors `{error:"..."}`. All timestamps UNIX milliseconds. Persistent SQLite database defaults `data/system.db`. Text understanding and candidate scoring use a real configured model API by default. Moderation, action decisions and comment generation remain simulated. Raw media are not perceived; supplied captions are text inputs. `AI_MODE=test_fixture` explicitly enables offline deterministic fixtures.

- `GET /api/health`: `{status,mode,semantics,remaining_simulations,budget_unit}`; mode is REAL_SEMANTICS or TEST_FIXTURE; credentials and endpoint are never returned.
- `GET /api/avatars`: `{avatars:[{id,name,persona,interests,active,public_memories:[]}]}`; private memories never returned.
- `PATCH /api/avatars/{id}`: `{active:false}` revokes avatar, epoch increments.
- `POST /api/uploads`: multipart `file` and `caption`; up to 20 MiB; image/audio/video accepted. Returns `{id,name,mime,size,caption,url}`. `/api/uploads/{id}` authenticated by demo tenant header.
- `POST /api/posts`: header `Idempotency-Key` required. Body `{text:"...",attachment_ids:[],max_candidates:4, fault:"",deadline_seconds:120}`. Fault options `retry_once`, `always_fail`, `unknown_once`, `comment_reject`, `slow`. Returns 202 `{post_id,job_id,status:"ACCEPTED"}`; same key+input returns same IDs, changed input 409.
- `GET /api/posts`: `{posts:[{id,text,version,status,created_at,job_id,deleted,attachments:[],interactions:[]}]}` newest first.
- `GET /api/posts/{id}`: same post object; deleted returns 410.
- `DELETE /api/posts/{id}`: `{status:"DELETED"}`; cancels job and hides interactions atomically.
- `GET /api/jobs`: `{jobs:[job...]}`.
- `GET /api/jobs/{id}`: `{id,post_id,status,stage,attempts,error,created_at,updated_at,deadline,epoch,next_run,stages:[],candidates:[],operations:[],ai_operations:[],events:[]}`. Stages have name/status/output; candidates have avatar_id,name,score,evidence,actions,status,reason,comment. Operations expose operation_key,status,attempt,cost,error (no private prompts). Stages: MODERATE, UNDERSTAND, MATCH, DECIDE, PUBLISH, DONE. Terminal states COMPLETED/PARTIAL_COMPLETED/REJECTED/FAILED/CANCELLED/EXPIRED; retry WAITING_DEPENDENCY; unknown NEEDS_RECONCILIATION.
- `POST /api/jobs/{id}/retry`: retry FAILED/EXPIRED job with new epoch/deadline, reuses completed stages; does not revive deleted posts.
- `POST /api/jobs/{id}/reconcile`: reconcile simulated unknown provider receipt; returns `{status:"ACCEPTED"}`. An unknown simulation has a persisted provider receipt; re-running never double charges that request.
- `GET /api/stats`: `{posts,jobs,interactions,queue_depth,statuses:{...},budget:{limit,spent,reserved},mode:"REAL_SEMANTICS"}`.
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
- Budget values are integer request allowances, one per AI or simulated operation attempt. Real reported input/output tokens are stored separately; allowances do not represent currency or vendor invoices.

### Deterministic mock action examples
For relevant text such as `徒步旅行摄影`, the default mock plan is LIKE + COMMENT. Adding `只评论` selects COMMENT only, `只点赞` selects LIKE only, and `不需要互动` selects an empty plan. These are explicit local mock-model demonstration rules; they never bypass authorization, budget, moderation, or action uniqueness. These action rules remain simulated even when understanding and semantic matching use the real model.

## Real semantic provider operations

- `ai_operations` exposes operation_key, name, attempt, status, model label, response_id, input_tokens, output_tokens, usage_reported and a sanitized error. Request snapshots, endpoint and credentials are excluded. Accounting metadata remains readable for deleted jobs; post content and stages remain hidden.
- UNDERSTAND uses `understand-v1`; MATCH uses `match-v1`. Successful structured output is durably cached. Timeout, disconnect and recovered RUNNING become UNKNOWN; ordinary retry cannot automatically resend them.
- `POST /api/jobs/{id}/resolve-ai`: `{operation_key:"...",resolution:"abandon_and_charge"}` explicitly abandons an UNKNOWN attempt (or orphan RUNNING on an already terminal job), moves one request unit from reserved to spent and preserves deleted/superseded/expired states. An active unknown job becomes FAILED. No new model request is sent by this endpoint. A subsequent explicit retry can incur another provider charge. This is local accounting, not proof of provider billing, success, refund or reconciliation.
- `/reconcile` remains scoped to simulated operations and cannot resolve real AI UNKNOWN.
- Known 429/5xx failures use bounded workflow retries, with each attempt consuming a request unit conservatively. Other HTTP rejection and invalid structured output stop; no keyword/hash fallback occurs. Correct the cause and create a new post after non-retryable protocol/configuration errors.
- Full profile scoring is bounded to 50 active public profiles and JSON source validation; see [real semantics](real-semantics.md).
