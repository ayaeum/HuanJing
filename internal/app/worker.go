package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Candidate struct {
	AvatarID     string            `json:"avatar_id"`
	Name         string            `json:"name"`
	Score        float64           `json:"score"`
	Evidence     []string          `json:"evidence"`
	Actions      []string          `json:"actions"`
	Status       string            `json:"status"`
	Reason       string            `json:"reason"`
	Comment      string            `json:"comment"`
	Epoch        int               `json:"-"`
	Sources      []string          `json:"-"`
	ActionStates map[string]string `json:"action_states"`
}

func (a *App) candidates(job string) ([]Candidate, error) {
	rs, e := a.DB.Query(`SELECT avatar_id,name,score,evidence,actions,status,reason,comment,avatar_epoch,sources FROM candidates WHERE job_id=? ORDER BY score DESC,avatar_id`, job)
	if e != nil {
		return nil, e
	}
	out := []Candidate{}
	for rs.Next() {
		var c Candidate
		var ev, ac, so string
		if e = rs.Scan(&c.AvatarID, &c.Name, &c.Score, &ev, &ac, &c.Status, &c.Reason, &c.Comment, &c.Epoch, &so); e != nil {
			rs.Close()
			return nil, e
		}
		json.Unmarshal([]byte(ev), &c.Evidence)
		json.Unmarshal([]byte(ac), &c.Actions)
		json.Unmarshal([]byte(so), &c.Sources)
		out = append(out, c)
	}
	rs.Close()
	for i := range out {
		out[i].ActionStates = map[string]string{}
		rows, e := a.DB.Query(`SELECT kind,status FROM action_states WHERE job_id=? AND avatar_id=?`, job, out[i].AvatarID)
		if e == nil {
			for rows.Next() {
				var k, s string
				rows.Scan(&k, &s)
				out[i].ActionStates[k] = s
			}
			rows.Close()
		}
	}
	return out, nil
}

type flowError struct{ Status, Message string }

func (e *flowError) Error() string { return e.Message }
func (a *App) claim() (Job, error) {
	tx, e := a.DB.Begin()
	if e != nil {
		return Job{}, e
	}
	defer tx.Rollback()
	t := now()
	_, e = tx.Exec(`UPDATE jobs SET status='EXPIRED',error='business deadline reached',lease=0,epoch=epoch+1,updated=? WHERE deadline<=? AND status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY')`, t, t)
	if e != nil {
		return Job{}, e
	}
	j, e := scanJob(tx.QueryRow("SELECT "+jobcols+" FROM jobs WHERE status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY') AND lease<? AND next_run<=? AND deadline>? ORDER BY updated,id LIMIT 1", t, t, t))
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			if ce := tx.Commit(); ce != nil {
				return j, ce
			}
		}
		return j, e
	}
	j.Epoch++
	j.Attempts++
	j.Status = "PROCESSING"
	_, e = tx.Exec(`UPDATE jobs SET status='PROCESSING',epoch=?,attempts=?,lease=?,updated=? WHERE id=?`, j.Epoch, j.Attempts, t+3000, t, j.ID)
	if e != nil {
		return j, e
	}
	e = tx.Commit()
	return j, e
}
func (a *App) worker(ctx context.Context) {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j, e := a.claim()
			if e != nil {
				continue
			}
			a.Active.Add(1)
			done := make(chan struct{})
			go func() {
				tick := time.NewTicker(time.Second)
				defer tick.Stop()
				for {
					select {
					case <-done:
						return
					case <-ctx.Done():
						return
					case <-tick.C:
						a.DB.Exec(`UPDATE jobs SET lease=? WHERE id=? AND epoch=? AND status='PROCESSING'`, now()+3000, j.ID, j.Epoch)
					}
				}
			}()
			e = a.step(ctx, j)
			close(done)
			a.Active.Add(-1)
			if e != nil {
				a.fail(j, e)
			}
		}
	}
}
func (a *App) fail(j Job, e error) {
	s := "WAITING_DEPENDENCY"
	fail := j.Failures + 1
	var fe *flowError
	if errors.As(e, &fe) {
		s = fe.Status
	}
	if s == "WAITING_DEPENDENCY" && fail >= 3 {
		s = "FAILED"
	}
	delay := int64(300 * (1 << min(fail, 4)))
	a.DB.Exec(`UPDATE jobs SET status=?,failures=?,error=?,next_run=?,lease=0,updated=? WHERE id=? AND epoch=? AND status='PROCESSING'`, s, fail, e.Error(), now()+delay, now(), j.ID, j.Epoch)
}
func (a *App) saveStage(j Job, next string, data any) error {
	tx, e := a.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = valid(tx, j); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO stages VALUES(?,?,?,?)`, j.ID, j.Stage, js(data), now()); e != nil {
		return e
	}
	status := "ACCEPTED"
	if next == "DONE" {
		status = "COMPLETED"
		var bad int
		tx.QueryRow(`SELECT COUNT(*) FROM candidates WHERE job_id=? AND status NOT IN ('COMPLETED','IGNORED')`, j.ID).Scan(&bad)
		if bad > 0 {
			status = "PARTIAL_COMPLETED"
			var good int
			if e = tx.QueryRow(`SELECT (SELECT COUNT(*) FROM action_states WHERE job_id=? AND status='SUCCEEDED')+(SELECT COUNT(*) FROM candidates WHERE job_id=? AND status='IGNORED')`, j.ID, j.ID).Scan(&good); e != nil {
				return e
			}
			if good == 0 {
				status = "FAILED"
			}
		}
	}
	_, e = tx.Exec(`UPDATE jobs SET stage=?,status=?,lease=0,next_run=0,failures=0,error='',updated=? WHERE id=? AND epoch=?`, next, status, now(), j.ID, j.Epoch)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (a *App) content(j Job) (string, error) {
	var text, att string
	if e := a.DB.QueryRow(`SELECT text,attachments FROM posts WHERE id=? AND tenant=? AND deleted=0`, j.PostID, j.Tenant).Scan(&text, &att); e != nil {
		return "", e
	}
	var ids []string
	json.Unmarshal([]byte(att), &ids)
	for _, id := range ids {
		var caption string
		if e := a.DB.QueryRow(`SELECT caption FROM uploads WHERE id=? AND tenant=?`, id, j.Tenant).Scan(&caption); e != nil {
			return "", e
		}
		if caption == "" {
			return "", &flowError{"REJECTED", "MOCK media requires caption; raw media has not been perceived or audited"}
		}
		text += "\n[MOCK media caption] " + caption
	}
	return text, nil
}
func (a *App) step(ctx context.Context, j Job) error {
	switch j.Stage {
	case "MODERATE":
		text, e := a.content(j)
		if e != nil {
			return e
		}
		lower := strings.ToLower(text)
		for _, x := range []string{"[reject]", "violent threat", "违禁测试", "暴力威胁", "杀死所有"} {
			if strings.Contains(lower, x) {
				return &flowError{"REJECTED", "mock moderation rejected configured unsafe marker"}
			}
		}
		return a.saveStage(j, "UNDERSTAND", map[string]any{"verdict": "PASS", "engine": "deterministic_mock", "scope": "text and supplied captions only; not real media moderation"})
	case "UNDERSTAND":
		if a.AIMode != "TEST_FIXTURE" {
			return a.understandReal(ctx, j)
		}
		text, e := a.content(j)
		if e != nil {
			return e
		}
		out, e := a.call(ctx, j, "understand", "", js(map[string]any{"summary": text, "topics": fixtureTopics(text), "model": "mock-v1", "embedding": "hash-bigram-64-v1"}))
		if e != nil {
			return e
		}
		var data any
		json.Unmarshal([]byte(out), &data)
		return a.saveStage(j, "MATCH", data)
	case "MATCH":
		var err error
		if a.AIMode == "TEST_FIXTURE" {
			err = a.matchFixture(j)
		} else {
			err = a.matchReal(ctx, j)
		}
		if e := err; e != nil {
			return e
		}
		return a.saveStage(j, "DECIDE", map[string]any{"strategy": "topic-qualified hash similarity + keyword + public history", "max_candidates": j.Max})
	case "DECIDE":
		if e := a.decide(ctx, j); e != nil {
			return e
		}
		return a.saveStage(j, "PUBLISH", map[string]any{"private_context": "used only inside decision; never copied into public generation", "plan": "LIKE and COMMENT frozen independently"})
	case "PUBLISH":
		if e := a.publish(ctx, j); e != nil {
			return e
		}
		return a.saveStage(j, "DONE", map[string]any{"actions": "independently idempotent", "publisher": "local SQL transaction"})
	}
	return &flowError{"FAILED", "unknown stage"}
}

// call simulates an idempotent external provider with durable receipts. Each new
// billable attempt reserves one mock unit; lost receipts retain their reservation.
func (a *App) call(ctx context.Context, j Job, name, av, result string) (string, error) {
	base := j.ID + ":" + name + ":" + av
	tx, e := a.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if e = valid(tx, j); e != nil {
		return "", e
	}
	var key, status, res, receipt string
	var attempt int
	e = tx.QueryRow(`SELECT operation_key,status,result,provider_result,attempt FROM operations WHERE job_id=? AND name=? AND avatar_id=? ORDER BY attempt DESC LIMIT 1`, j.ID, name, av).Scan(&key, &status, &res, &receipt, &attempt)
	if e == nil {
		if status == "SUCCEEDED" {
			return res, nil
		}
		if status == "UNKNOWN" {
			return "", &flowError{"NEEDS_RECONCILIATION", "provider reply unknown; reconcile durable mock receipt"}
		}
		if status == "RUNNING" { // crash recovery queries provider receipt rather than blindly billing again
			if receipt != "" {
				_, e = tx.Exec(`UPDATE operations SET status='SUCCEEDED',result=provider_result WHERE operation_key=?`, key)
				if e == nil {
					_, e = tx.Exec(`UPDATE budgets SET reserved=reserved-1,spent=spent+1 WHERE tenant=?`, j.Tenant)
				}
				if e == nil {
					e = tx.Commit()
				}
				return receipt, e
			}
			return "", &flowError{"NEEDS_RECONCILIATION", "missing provider receipt"}
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	attempt++
	key = fmt.Sprintf("%s:%d", base, attempt)
	q, e := tx.Exec(`UPDATE budgets SET reserved=reserved+1 WHERE tenant=? AND spent+reserved+1<=limit_units`, j.Tenant)
	if e != nil {
		return "", e
	}
	n, _ := q.RowsAffected()
	if n == 0 {
		return "", &flowError{"WAITING_DEPENDENCY", "mock cost budget exhausted"}
	}
	_, e = tx.Exec(`INSERT INTO operations VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, key, j.Tenant, j.ID, av, name, attempt, "RUNNING", 1, "", "", result, now())
	if e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	delay := 30 * time.Millisecond
	if j.Fault == "slow" {
		delay = 1800 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(delay):
	}
	final := "SUCCEEDED"
	er := ""
	if name == "understand" && (j.Fault == "always_fail" || (j.Fault == "retry_once" && attempt == 1)) {
		final = "FAILED"
		er = "simulated transient provider 503"
	}
	if name == "comment" && j.Fault == "unknown_once" && attempt == 1 {
		final = "UNKNOWN"
		er = "simulated lost provider response; receipt persisted"
	}
	tx, e = a.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	q, e = tx.Exec(`UPDATE operations SET status=?,result=?,error=? WHERE operation_key=? AND status='RUNNING'`, final, result, er, key)
	if e != nil {
		return "", e
	}
	n, _ = q.RowsAffected()
	if n > 0 && final != "UNKNOWN" {
		_, e = tx.Exec(`UPDATE budgets SET reserved=reserved-1,spent=spent+1 WHERE tenant=?`, j.Tenant)
		if e != nil {
			return "", e
		}
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	if final == "FAILED" {
		return "", errors.New(er)
	}
	if final == "UNKNOWN" {
		return "", &flowError{"NEEDS_RECONCILIATION", er}
	}
	return result, nil
}

func (a *App) decide(ctx context.Context, j Job) error {
	publicText, contentErr := a.content(j)
	if contentErr != nil {
		return contentErr
	}
	cs, e := a.candidates(j.ID)
	if e != nil {
		return e
	}
	for _, c := range cs {
		if c.Status != "PENDING" {
			continue
		}
		sources := []string{}
		privateCount := 0
		rs, e := a.DB.Query(`SELECT m.id,m.public FROM memories m WHERE tenant=? AND avatar_id=? AND revoked=0 AND (source_post='' OR EXISTS(SELECT 1 FROM posts p WHERE p.id=m.source_post AND p.deleted=0)) LIMIT 8`, j.Tenant, c.AvatarID)
		if e != nil {
			return e
		}
		for rs.Next() {
			var id string
			var public int
			rs.Scan(&id, &public)
			sources = append(sources, id)
			if public == 0 {
				privateCount++
			}
		}
		rs.Close()
		actions := []string{}
		reason := "LOW_RELEVANCE"
		if strings.Contains(strings.Join(c.Evidence, " "), "keyword rank=") || strings.Contains(strings.Join(c.Evidence, " "), "real_model relevance=") {
			actions = []string{"LIKE", "COMMENT"}
			reason = "SHARED_INTEREST"
		} else if (strings.Contains(strings.Join(c.Evidence, " "), "semantic rank=") || strings.Contains(strings.Join(c.Evidence, " "), "public_history rank=")) && privateCount > 0 {
			actions = []string{"LIKE"}
			reason = "PERSONA_CONTEXT_FIT"
		}
		// Explicit deterministic demo rules, not tool authority from untrusted text.
		if strings.Contains(publicText, "不需要互动") {
			actions = []string{}
			reason = "MOCK_NO_INTERACTION"
		} else if len(actions) > 0 && strings.Contains(publicText, "只评论") {
			actions = []string{"COMMENT"}
			reason = "MOCK_COMMENT_ONLY"
		} else if len(actions) > 0 && strings.Contains(publicText, "只点赞") {
			actions = []string{"LIKE"}
			reason = "MOCK_LIKE_ONLY"
		}
		snapshot := struct {
			Actions []string `json:"actions"`
			Sources []string `json:"sources"`
			Reason  string   `json:"reason"`
		}{actions, sources, reason}
		out, e := a.call(ctx, j, "decision", c.AvatarID, js(snapshot))
		if e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(out), &snapshot); e != nil {
			return e
		}
		actions, sources, reason = snapshot.Actions, snapshot.Sources, snapshot.Reason
		tx, e := a.DB.Begin()
		if e != nil {
			return e
		}
		if e = valid(tx, j); e != nil {
			tx.Rollback()
			return e
		}
		_, e = tx.Exec(`INSERT OR IGNORE INTO decisions(tenant,post_id,avatar_id,actions) VALUES(?,?,?,?)`, j.Tenant, j.PostID, c.AvatarID, js(actions))
		var frozen string
		if e == nil {
			e = tx.QueryRow(`SELECT actions FROM decisions WHERE tenant=? AND post_id=? AND avatar_id=?`, j.Tenant, j.PostID, c.AvatarID).Scan(&frozen)
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		json.Unmarshal([]byte(frozen), &actions)
		_, e = tx.Exec(`UPDATE candidates SET actions=?,reason=?,status='READY',sources=? WHERE job_id=? AND avatar_id=? AND status='PENDING'`, js(actions), reason, js(sources), j.ID, c.AvatarID)
		for _, k := range actions {
			if e == nil {
				_, e = tx.Exec(`INSERT OR IGNORE INTO action_states(job_id,avatar_id,kind,status) VALUES(?,?,?,'PENDING')`, j.ID, c.AvatarID, k)
			}
		}
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
		if e != nil {
			return e
		}
	}
	return nil
}
func (a *App) publish(ctx context.Context, j Job) error {
	cs, e := a.candidates(j.ID)
	if e != nil {
		return e
	}
	for _, c := range cs {
		if c.Status == "COMPLETED" || c.Status == "IGNORED" || c.Status == "STALE" {
			continue
		}
		if len(c.Actions) == 0 {
			tx, e := a.DB.Begin()
			if e != nil {
				return e
			}
			if e = valid(tx, j); e == nil {
				status := "IGNORED"
				allowed, checkErr := candidateAuthorized(tx, j, c)
				if checkErr != nil {
					tx.Rollback()
					return checkErr
				}
				if !allowed {
					status = "STALE"
				}
				_, e = tx.Exec(`UPDATE candidates SET status=? WHERE job_id=? AND avatar_id=?`, status, j.ID, c.AvatarID)
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
			if e != nil {
				return e
			}
			continue
		}
		partial := false
		for _, kind := range c.Actions {
			var st string
			a.DB.QueryRow(`SELECT status FROM action_states WHERE job_id=? AND avatar_id=? AND kind=?`, j.ID, c.AvatarID, kind).Scan(&st)
			if st == "SUCCEEDED" {
				continue
			}
			if st == "REJECTED" || st == "STALE" {
				partial = true
				continue
			}
			comment := ""
			if kind == "COMMENT" {
				if j.Fault == "comment_reject" {
					if _, err := a.DB.Exec(`UPDATE action_states SET status='REJECTED',error='mock output moderation rejected' WHERE job_id=? AND avatar_id=? AND kind=?`, j.ID, c.AvatarID, kind); err != nil {
						return err
					}
					partial = true
					continue
				}
				var interest string
				a.DB.QueryRow(`SELECT interests FROM avatars WHERE tenant=? AND id=?`, j.Tenant, c.AvatarID).Scan(&interest)
				words := strings.Fields(interest)
				topic := "这个话题"
				if len(words) > 0 {
					topic = words[0]
				}
				comment = fmt.Sprintf("关于%s的分享很有意思！你最想交流的是哪一部分？", topic)
				comment, e = a.call(ctx, j, "comment", c.AvatarID, comment)
				if e != nil {
					var fe *flowError
					if errors.As(e, &fe) && fe.Status == "NEEDS_RECONCILIATION" {
						if _, err := a.DB.Exec(`UPDATE action_states SET status='UNKNOWN',error=? WHERE job_id=? AND avatar_id=? AND kind=?`, e.Error(), j.ID, c.AvatarID, kind); err != nil {
							return err
						}
					}
					return e
				}
			}
			ok, e := a.commitAction(j, c, kind, comment)
			if e != nil {
				return e
			}
			if !ok {
				partial = true
			}
		}
		status := "COMPLETED"
		if partial {
			status = "PARTIAL"
			var successes int
			if e = a.DB.QueryRow(`SELECT COUNT(*) FROM action_states WHERE job_id=? AND avatar_id=? AND status='SUCCEEDED'`, j.ID, c.AvatarID).Scan(&successes); e != nil {
				return e
			}
			if successes == 0 {
				status = "FAILED"
			}
		}
		if _, err := a.DB.Exec(`UPDATE candidates SET status=? WHERE job_id=? AND avatar_id=? AND EXISTS(SELECT 1 FROM jobs WHERE jobs.id=? AND epoch=? AND status='PROCESSING')`, status, j.ID, c.AvatarID, j.ID, j.Epoch); err != nil {
			return err
		}
	}
	return nil
}
func candidateAuthorized(tx *sql.Tx, j Job, c Candidate) (bool, error) {
	var active, epoch int
	err := tx.QueryRow(`SELECT active,epoch FROM avatars WHERE tenant=? AND id=?`, j.Tenant, c.AvatarID).Scan(&active, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if active != 1 || epoch != c.Epoch {
		return false, nil
	}
	for _, id := range c.Sources {
		var revoked, deleted int
		err = tx.QueryRow(`SELECT m.revoked,CASE WHEN m.source_post<>'' AND p.id IS NULL THEN 1 ELSE COALESCE(p.deleted,0) END FROM memories m LEFT JOIN posts p ON p.id=m.source_post WHERE m.id=? AND m.tenant=? AND m.avatar_id=?`, id, j.Tenant, c.AvatarID).Scan(&revoked, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if revoked != 0 || deleted != 0 {
			return false, nil
		}
	}
	return true, nil
}
func (a *App) commitAction(j Job, c Candidate, kind, comment string) (bool, error) {
	tx, e := a.DB.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if e = valid(tx, j); e != nil {
		return false, e
	}
	allowed, e := candidateAuthorized(tx, j, c)
	if e != nil {
		return false, e
	}
	stale := !allowed
	if stale {
		_, e = tx.Exec(`UPDATE action_states SET status='STALE',error='authorization or source revoked' WHERE job_id=? AND avatar_id=? AND kind=?`, j.ID, c.AvatarID, kind)
		if e == nil {
			_, e = tx.Exec(`UPDATE candidates SET status='STALE' WHERE job_id=? AND avatar_id=?`, j.ID, c.AvatarID)
		}
		if e == nil {
			e = tx.Commit()
		}
		return false, e
	}
	var existing string
	e = tx.QueryRow(`SELECT id FROM interactions WHERE tenant=? AND post_id=? AND avatar_id=? AND kind=?`, j.Tenant, j.PostID, c.AvatarID, kind).Scan(&existing)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false, e
	}
	if existing == "" {
		existing = ID("i_")
		_, e = tx.Exec(`INSERT INTO interactions VALUES(?,?,?,?,?,?,?)`, existing, j.Tenant, j.PostID, c.AvatarID, kind, comment, now())
		if e != nil {
			return false, e
		}
		_, e = tx.Exec(`INSERT INTO outbox(id,tenant,post_id,interaction_id,avatar_id,content,created) VALUES(?,?,?,?,?,?,?)`, ID("e_"), j.Tenant, j.PostID, existing, c.AvatarID, "我在一篇公开帖子中做出了"+kind+"互动。", now())
		if e != nil {
			return false, e
		}
	}
	_, e = tx.Exec(`UPDATE action_states SET status='SUCCEEDED',interaction_id=?,error='' WHERE job_id=? AND avatar_id=? AND kind=?`, existing, j.ID, c.AvatarID, kind)
	if e == nil && kind == "COMMENT" {
		_, e = tx.Exec(`UPDATE candidates SET comment=? WHERE job_id=? AND avatar_id=?`, comment, j.ID, c.AvatarID)
	}
	if e != nil {
		return false, e
	}
	return true, tx.Commit()
}
func (a *App) events(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for i := 0; i < 32; i++ {
				if ctx.Err() != nil || !a.projectEvent() {
					break
				}
			}
		}
	}
}
func (a *App) projectEvent() bool {
	tx, e := a.DB.Begin()
	if e != nil {
		return false
	}
	defer tx.Rollback()
	var id, tenant, post, interaction, av, content string
	e = tx.QueryRow(`SELECT id,tenant,post_id,interaction_id,avatar_id,content FROM outbox WHERE status='PENDING' ORDER BY created LIMIT 1`).Scan(&id, &tenant, &post, &interaction, &av, &content)
	if e != nil {
		return false
	}
	var deleted int
	e = tx.QueryRow(`SELECT deleted FROM posts WHERE id=? AND tenant=?`, post, tenant).Scan(&deleted)
	status := "DELIVERED"
	if errors.Is(e, sql.ErrNoRows) || deleted != 0 {
		status = "SKIPPED_SOURCE_REVOKED"
		e = nil
	} else if e == nil {
		_, e = tx.Exec(`INSERT OR IGNORE INTO memories(id,tenant,avatar_id,content,public,source_post,source_interaction) VALUES(?,?,?,?,0,?,?)`, ID("m_"), tenant, av, content, post, interaction)
	}
	if e == nil {
		_, e = tx.Exec(`UPDATE outbox SET status=? WHERE id=?`, status, id)
	}
	if e != nil {
		return false
	}
	return tx.Commit() == nil
}
