package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func tenant(r *http.Request) string {
	t := r.Header.Get("X-Tenant-ID")
	if t == "" {
		return "demo"
	}
	return t
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func bad(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		bad(w, 400, err.Error())
		return false
	}
	var more any
	if d.Decode(&more) != io.EOF {
		bad(w, 400, "one JSON value required")
		return false
	}
	return true
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.DB.PingContext(ctx); err != nil {
			bad(w, 503, "database unavailable")
			return
		}
		respond(w, 200, map[string]string{"status": "ok", "mode": "LOCAL_MOCK"})
	})
	m.HandleFunc("GET /api/avatars", a.avatars)
	m.HandleFunc("PATCH /api/avatars/{id}", a.avatarPatch)
	m.HandleFunc("POST /api/uploads", a.upload)
	m.HandleFunc("GET /api/uploads/{id}", a.uploadGet)
	m.HandleFunc("POST /api/posts", a.createPost)
	m.HandleFunc("GET /api/posts", a.posts)
	m.HandleFunc("GET /api/posts/{id}", a.post)
	m.HandleFunc("DELETE /api/posts/{id}", a.deletePost)
	m.HandleFunc("PATCH /api/posts/{id}", a.editPost)
	m.HandleFunc("GET /api/memories", a.memories)
	m.HandleFunc("DELETE /api/memories/{id}", a.revokeMemory)
	m.HandleFunc("GET /api/jobs", a.jobs)
	m.HandleFunc("GET /api/jobs/{id}", a.job)
	m.HandleFunc("POST /api/jobs/{id}/retry", a.retry)
	m.HandleFunc("POST /api/jobs/{id}/reconcile", a.reconcile)
	m.HandleFunc("GET /api/stats", a.stats)
	m.HandleFunc("POST /api/budget", a.budget)
	m.HandleFunc("GET /metrics", a.metrics)
	m.Handle("/", http.FileServer(http.Dir(a.WebDir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				bad(w, 500, "internal server error")
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if len(tenant(r)) > 64 || strings.ContainsAny(tenant(r), "/\\\x00") {
			bad(w, 400, "invalid tenant")
			return
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) avatars(w http.ResponseWriter, r *http.Request) {
	rows, e := a.DB.Query(`SELECT id,name,persona,interests,active FROM avatars WHERE tenant=? ORDER BY id`, tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	out := []map[string]any{}
	for rows.Next() {
		var id, n, p, i string
		var active int
		rows.Scan(&id, &n, &p, &i, &active)
		out = append(out, map[string]any{"id": id, "name": n, "persona": p, "interests": i, "active": active == 1, "public_memories": []string{}})
	}
	rows.Close()
	for _, v := range out {
		rs, _ := a.DB.Query(`SELECT content FROM memories m WHERE tenant=? AND avatar_id=? AND public=1 AND revoked=0 AND (source_post='' OR EXISTS(SELECT 1 FROM posts p WHERE p.id=m.source_post AND p.deleted=0))`, tenant(r), v["id"])
		if rs != nil {
			mem := []string{}
			for rs.Next() {
				var s string
				rs.Scan(&s)
				mem = append(mem, s)
			}
			rs.Close()
			v["public_memories"] = mem
		}
	}
	respond(w, 200, map[string]any{"avatars": out})
}
func (a *App) avatarPatch(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Active *bool `json:"active"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Active == nil {
		bad(w, 400, "active required")
		return
	}
	v := 0
	if *b.Active {
		v = 1
	}
	res, e := a.DB.Exec(`UPDATE avatars SET active=?,epoch=epoch+1 WHERE tenant=? AND id=?`, v, tenant(r), r.PathValue("id"))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		bad(w, 404, "avatar not found")
		return
	}
	respond(w, 200, map[string]any{"active": *b.Active})
}
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 21<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		bad(w, 400, "multipart upload exceeds limits or is invalid")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, h, e := r.FormFile("file")
	if e != nil {
		bad(w, 400, "file required")
		return
	}
	defer f.Close()
	if h.Size > 20<<20 {
		bad(w, 413, "maximum file size 20 MiB")
		return
	}
	caption := strings.TrimSpace(r.FormValue("caption"))
	if len(caption) > 4000 {
		bad(w, 400, "caption too long")
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	f.Seek(0, 0)
	mt := http.DetectContentType(head[:n])
	if !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "audio/") && !strings.HasPrefix(mt, "video/") {
		if mt == "application/ogg" {
			mt = "audio/ogg"
		} else if n >= 12 && string(head[4:8]) == "ftyp" {
			mt = "video/mp4"
		} else if n >= 4 && head[0] == 0x1a && head[1] == 0x45 && head[2] == 0xdf && head[3] == 0xa3 {
			mt = "video/webm"
		} else {
			bad(w, 415, "supported media signatures: image, audio, video; unknown binary rejected")
			return
		}

	}
	id := ID("u_")
	path := filepath.Join(a.UploadDir, id)
	dest, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	size, e := io.Copy(dest, io.LimitReader(f, (20<<20)+1))
	dest.Close()
	if e != nil || size > 20<<20 {
		os.Remove(path)
		bad(w, 413, "upload failed or too large")
		return
	}
	_, e = a.DB.Exec(`INSERT INTO uploads VALUES(?,?,?,?,?,?,?,?)`, id, tenant(r), filepath.Base(h.Filename), mt, size, caption, path, now())
	if e != nil {
		os.Remove(path)
		bad(w, 500, e.Error())
		return
	}
	respond(w, 201, map[string]any{"id": id, "name": filepath.Base(h.Filename), "mime": mt, "size": size, "caption": caption, "url": "/api/uploads/" + id})
}
func (a *App) uploadGet(w http.ResponseWriter, r *http.Request) {
	var path, mt, name string
	if a.DB.QueryRow(`SELECT path,mime,name FROM uploads WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r)).Scan(&path, &mt, &name) != nil {
		bad(w, 404, "upload not found")
		return
	}
	// Unattached uploads remain previewable. Once attached, access requires a
	// live parent; shared attachments survive deletion of just one parent.
	var parents, live int
	if err := a.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN p.deleted=0 AND EXISTS(SELECT 1 FROM json_each(p.attachments) a WHERE a.value=b.upload_id) THEN 1 ELSE 0 END),0) FROM upload_bindings b JOIN posts p ON p.id=b.post_id AND p.tenant=b.tenant WHERE b.tenant=? AND b.upload_id=?`, tenant(r), r.PathValue("id")).Scan(&parents, &live); err != nil {
		bad(w, 500, err.Error())
		return
	}
	if parents > 0 && live == 0 {
		bad(w, 410, "attachment parent deleted")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	http.ServeFile(w, r, path)
}

type createInput struct {
	Text        string   `json:"text"`
	Attachments []string `json:"attachment_ids"`
	Max         int      `json:"max_candidates"`
	Fault       string   `json:"fault"`
	Deadline    int      `json:"deadline_seconds"`
}

func (a *App) createPost(w http.ResponseWriter, r *http.Request) {
	var b createInput
	if !decode(w, r, &b) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		bad(w, 400, "Idempotency-Key required, max 128 bytes")
		return
	}
	b.Text = strings.TrimSpace(b.Text)
	if len(b.Text) > 16000 || len(b.Attachments) > 4 || (b.Text == "" && len(b.Attachments) == 0) {
		bad(w, 400, "text or attachment required, max text 16000 bytes and 4 attachments")
		return
	}
	if b.Max == 0 {
		b.Max = 4
	}
	if b.Max < 1 || b.Max > 5 {
		bad(w, 400, "max_candidates 1..5")
		return
	}
	if b.Deadline == 0 {
		b.Deadline = 120
	}
	if b.Deadline < 2 || b.Deadline > 600 {
		bad(w, 400, "deadline_seconds 2..600")
		return
	}
	switch b.Fault {
	case "", "retry_once", "always_fail", "unknown_once", "comment_reject", "slow":
	default:
		bad(w, 400, "invalid fault")
		return
	}
	hash := sha256.Sum256([]byte(js(b)))
	digest := hex.EncodeToString(hash[:])
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	var oldhash, pid, jid string
	e = tx.QueryRow(`SELECT hash,post_id,job_id FROM request_keys WHERE tenant=? AND key=?`, tenant(r), key).Scan(&oldhash, &pid, &jid)
	if e == nil {
		if oldhash != digest {
			bad(w, 409, "idempotency key reused with different input")
			return
		}
		respond(w, 202, map[string]string{"post_id": pid, "job_id": jid, "status": "ACCEPTED"})
		return
	}
	if !errors.Is(e, sql.ErrNoRows) {
		bad(w, 500, e.Error())
		return
	}
	var pending int
	tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE tenant=? AND status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY','NEEDS_RECONCILIATION')`, tenant(r)).Scan(&pending)
	if pending >= 1000 {
		bad(w, 429, "tenant pending queue full")
		return
	}
	for _, id := range b.Attachments {
		var one int
		if tx.QueryRow(`SELECT 1 FROM uploads WHERE id=? AND tenant=?`, id, tenant(r)).Scan(&one) != nil {
			bad(w, 400, "unknown or unauthorized attachment")
			return
		}
	}
	pid, jid = ID("p_"), ID("j_")
	t := now()
	_, e = tx.Exec(`INSERT INTO posts(id,tenant,text,attachments,created) VALUES(?,?,?,?,?)`, pid, tenant(r), b.Text, js(b.Attachments), t)
	if e == nil {
		_, e = tx.Exec(`INSERT INTO jobs(id,tenant,post_id,status,stage,deadline,max_candidates,fault,created,updated) VALUES(?,?,?,'ACCEPTED','MODERATE',?,?,?,?,?)`, jid, tenant(r), pid, t+int64(b.Deadline)*1000, b.Max, b.Fault, t, t)
	}
	if e == nil {
		_, e = tx.Exec(`INSERT INTO request_keys VALUES(?,?,?,?,?)`, tenant(r), key, digest, pid, jid)
	}
	if e == nil {
		_, e = tx.Exec(`INSERT OR IGNORE INTO budgets(tenant) VALUES(?)`, tenant(r))
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	respond(w, 202, map[string]string{"post_id": pid, "job_id": jid, "status": "ACCEPTED"})
}
func (a *App) postObject(id, t string) (map[string]any, error) {
	var text, att, jid, status string
	var ver, del int
	var created int64
	e := a.DB.QueryRow(`SELECT p.text,p.attachments,p.version,p.deleted,p.created,j.id,j.status FROM posts p JOIN jobs j ON j.post_id=p.id WHERE p.id=? AND p.tenant=? ORDER BY j.created DESC,j.id DESC LIMIT 1`, id, t).Scan(&text, &att, &ver, &del, &created, &jid, &status)
	if e != nil {
		return nil, e
	}
	var ids []string
	json.Unmarshal([]byte(att), &ids)
	attachments := []map[string]any{}
	for _, uid := range ids {
		var n, m, c string
		var s int64
		if a.DB.QueryRow(`SELECT name,mime,caption,size FROM uploads WHERE id=? AND tenant=?`, uid, t).Scan(&n, &m, &c, &s) == nil {
			attachments = append(attachments, map[string]any{"id": uid, "name": n, "mime": m, "caption": c, "size": s, "url": "/api/uploads/" + uid})
		}
	}
	inter := []map[string]any{}
	if del == 0 {
		rs, e := a.DB.Query(`SELECT i.id,i.avatar_id,a.name,i.kind,i.content,i.created FROM interactions i JOIN avatars a ON a.id=i.avatar_id AND a.tenant=i.tenant WHERE i.post_id=? AND i.tenant=? ORDER BY i.created`, id, t)
		if e == nil {
			for rs.Next() {
				var iid, aid, n, k, c string
				var ct int64
				rs.Scan(&iid, &aid, &n, &k, &c, &ct)
				inter = append(inter, map[string]any{"id": iid, "avatar_id": aid, "name": n, "kind": k, "content": c, "created_at": ct})
			}
			rs.Close()
		}
	}
	return map[string]any{"id": id, "text": text, "version": ver, "deleted": del == 1, "created_at": created, "job_id": jid, "status": status, "attachments": attachments, "interactions": inter}, nil
}
func (a *App) posts(w http.ResponseWriter, r *http.Request) {
	rs, e := a.DB.Query(`SELECT id FROM posts WHERE tenant=? AND deleted=0 ORDER BY created DESC LIMIT 100`, tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	ids := []string{}
	for rs.Next() {
		var id string
		rs.Scan(&id)
		ids = append(ids, id)
	}
	rs.Close()
	out := []map[string]any{}
	for _, id := range ids {
		p, e := a.postObject(id, tenant(r))
		if e == nil {
			out = append(out, p)
		}
	}
	respond(w, 200, map[string]any{"posts": out})
}
func (a *App) post(w http.ResponseWriter, r *http.Request) {
	p, e := a.postObject(r.PathValue("id"), tenant(r))
	if e != nil {
		bad(w, 404, "post not found")
		return
	}
	if p["deleted"] == true {
		bad(w, 410, "post deleted")
		return
	}
	respond(w, 200, p)
}
func (a *App) deletePost(w http.ResponseWriter, r *http.Request) {
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec(`UPDATE posts SET deleted=1,version=version+CASE WHEN deleted=0 THEN 1 ELSE 0 END WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		bad(w, 404, "post not found")
		return
	}
	_, e = tx.Exec(`UPDATE jobs SET status='CANCELLED',epoch=epoch+1,lease=0,error='post deleted',updated=? WHERE post_id=? AND tenant=?`, now(), r.PathValue("id"), tenant(r))
	if e == nil {
		_, e = tx.Exec(`UPDATE memories SET revoked=1 WHERE source_post=? AND tenant=?`, r.PathValue("id"), tenant(r))
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	respond(w, 200, map[string]string{"status": "DELETED"})
}
func (a *App) jobs(w http.ResponseWriter, r *http.Request) {
	rows, e := a.DB.Query("SELECT "+jobcols+" FROM jobs WHERE tenant=? ORDER BY created DESC LIMIT 100", tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e == nil {
			out = append(out, j)
		}
	}
	respond(w, 200, map[string]any{"jobs": out})
}
func (a *App) job(w http.ResponseWriter, r *http.Request) {
	j, e := a.Job(r.PathValue("id"), tenant(r))
	if e != nil {
		bad(w, 404, "job not found")
		return
	}
	var deleted int
	a.DB.QueryRow(`SELECT deleted FROM posts WHERE id=?`, j.PostID).Scan(&deleted)
	if deleted != 0 {
		respond(w, 200, map[string]any{"id": j.ID, "post_id": j.PostID, "status": j.Status, "stage": j.Stage, "error": "post deleted", "stages": []any{}, "candidates": []any{}, "operations": []any{}, "events": []any{}})
		return
	}
	raw, _ := json.Marshal(j)
	out := map[string]any{}
	json.Unmarshal(raw, &out)
	for _, k := range []string{"stages", "candidates", "operations", "events"} {
		out[k] = []any{}
	}
	rs, _ := a.DB.Query(`SELECT name,data FROM stages WHERE job_id=? ORDER BY created`, j.ID)
	if rs != nil {
		for rs.Next() {
			var n, d string
			rs.Scan(&n, &d)
			var v any
			json.Unmarshal([]byte(d), &v)
			out["stages"] = append(out["stages"].([]any), map[string]any{"name": n, "status": "SUCCEEDED", "output": v})
		}
		rs.Close()
	}
	cs, _ := a.candidates(j.ID)
	out["candidates"] = cs
	rs, _ = a.DB.Query(`SELECT operation_key,name,avatar_id,status,attempt,cost,error FROM operations WHERE job_id=? ORDER BY created`, j.ID)
	if rs != nil {
		for rs.Next() {
			var k, n, av, s, er string
			var attempt, cost int
			rs.Scan(&k, &n, &av, &s, &attempt, &cost, &er)
			out["operations"] = append(out["operations"].([]any), map[string]any{"operation_key": k, "name": n, "avatar_id": av, "status": s, "attempt": attempt, "cost": cost, "error": er})
		}
		rs.Close()
	}
	rs, _ = a.DB.Query(`SELECT id,status,interaction_id FROM outbox WHERE post_id=? AND tenant=?`, j.PostID, j.Tenant)
	if rs != nil {
		for rs.Next() {
			var id, s, i string
			rs.Scan(&id, &s, &i)
			out["events"] = append(out["events"].([]any), map[string]any{"id": id, "status": s, "interaction_id": i})
		}
		rs.Close()
	}
	respond(w, 200, out)
}
func (a *App) retry(w http.ResponseWriter, r *http.Request) {
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec(`UPDATE jobs SET status='ACCEPTED',failures=0,epoch=epoch+1,lease=0,next_run=0,deadline=?,error='',updated=? WHERE id=? AND tenant=? AND status IN ('FAILED','EXPIRED') AND stage<>'DONE' AND EXISTS(SELECT 1 FROM posts WHERE posts.id=jobs.post_id AND deleted=0)`, now()+120000, now(), r.PathValue("id"), tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		bad(w, 409, "only failed or expired active posts can retry; unknown requires reconciliation")
		return
	}
	if tx.Commit() != nil {
		bad(w, 500, "commit failed")
		return
	}
	respond(w, 202, map[string]string{"status": "ACCEPTED"})
}
func (a *App) reconcile(w http.ResponseWriter, r *http.Request) {
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	var status string
	if tx.QueryRow(`SELECT status FROM jobs WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r)).Scan(&status) != nil {
		bad(w, 404, "job not found")
		return
	}
	eligible := "status='UNKNOWN'"
	if status == "CANCELLED" || status == "SUPERSEDED" || status == "EXPIRED" || status == "FAILED" {
		eligible = "status IN ('UNKNOWN','RUNNING')"
	}
	var unknown int
	if e = tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id=? AND `+eligible+` AND provider_result<>''`, r.PathValue("id")).Scan(&unknown); e != nil {
		bad(w, 500, e.Error())
		return
	}
	if unknown == 0 {
		bad(w, 409, "no unresolved provider receipt")
		return
	}
	var cost int
	if e = tx.QueryRow(`SELECT COALESCE(SUM(cost),0) FROM operations WHERE job_id=? AND `+eligible+` AND provider_result<>''`, r.PathValue("id")).Scan(&cost); e != nil {
		bad(w, 500, e.Error())
		return
	}
	_, e = tx.Exec(`UPDATE operations SET status='SUCCEEDED',result=provider_result,error='' WHERE job_id=? AND `+eligible+` AND provider_result<>''`, r.PathValue("id"))
	if e == nil {
		_, e = tx.Exec(`UPDATE budgets SET reserved=reserved-?,spent=spent+? WHERE tenant=?`, cost, cost, tenant(r))
	}
	if e == nil && status == "NEEDS_RECONCILIATION" {
		_, e = tx.Exec(`UPDATE jobs SET status='ACCEPTED',epoch=epoch+1,lease=0,next_run=0,error='',deadline=?,updated=? WHERE id=? AND EXISTS(SELECT 1 FROM posts p WHERE p.id=jobs.post_id AND p.deleted=0 AND p.version=jobs.source_version)`, now()+120000, now(), r.PathValue("id"))
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	respond(w, 202, map[string]string{"status": "ACCEPTED"})
}
func (a *App) stats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"mode": "LOCAL_MOCK", "active_workers": a.Active.Load()}
	for _, table := range []string{"posts", "jobs", "interactions"} {
		var n int
		if e := a.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE tenant=?", tenant(r)).Scan(&n); e != nil {
			bad(w, 500, e.Error())
			return
		}
		out[table] = n
	}
	var q, l, spent, res, pending, done int
	queries := []struct {
		q    string
		args []any
		dst  []any
	}{
		{`SELECT COUNT(*) FROM jobs WHERE tenant=? AND status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY')`, []any{tenant(r)}, []any{&q}},
		{`SELECT COUNT(*) FROM outbox WHERE tenant=? AND status='PENDING'`, []any{tenant(r)}, []any{&pending}},
		{`SELECT COUNT(*) FROM outbox WHERE tenant=? AND status<>'PENDING'`, []any{tenant(r)}, []any{&done}},
	}
	for _, query := range queries {
		if e := a.DB.QueryRow(query.q, query.args...).Scan(query.dst...); e != nil {
			bad(w, 500, e.Error())
			return
		}
	}
	e := a.DB.QueryRow(`SELECT limit_units,spent,reserved FROM budgets WHERE tenant=?`, tenant(r)).Scan(&l, &spent, &res)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		bad(w, 500, e.Error())
		return
	}
	out["queue_depth"] = q
	out["outbox_pending"] = pending
	out["outbox_done"] = done
	out["budget"] = map[string]int{"limit": l, "spent": spent, "reserved": res}
	statuses := map[string]int{}
	rows, e := a.DB.Query(`SELECT status,COUNT(*) FROM jobs WHERE tenant=? GROUP BY status`, tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	for rows.Next() {
		var st string
		var n int
		if e = rows.Scan(&st, &n); e != nil {
			rows.Close()
			bad(w, 500, e.Error())
			return
		}
		statuses[st] = n
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	out["statuses"] = statuses
	respond(w, 200, out)
}
func (a *App) budget(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Limit int `json:"limit"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Limit < 0 || b.Limit > 10000000 {
		bad(w, 400, "limit 0..10000000")
		return
	}
	res, e := a.DB.Exec(`UPDATE budgets SET limit_units=? WHERE tenant=? AND spent+reserved<=?`, b.Limit, tenant(r), b.Limit)
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		bad(w, 409, "limit below spent+reserved or unknown tenant")
		return
	}
	respond(w, 200, map[string]int{"limit": b.Limit})
}
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	gauges := map[string]int64{"avatar_active_workers": a.Active.Load(), "avatar_server_time_seconds": time.Now().Unix()}
	for _, table := range []string{"posts", "jobs", "interactions", "operations", "outbox"} {
		var n int64
		if e := a.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
			bad(w, 503, "metrics database unavailable")
			return
		}
		gauges["avatar_"+table+"_total"] = n
	}
	queries := []struct{ name, query string }{
		{"avatar_queue_depth", `SELECT COUNT(*) FROM jobs WHERE status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY')`},
		{"avatar_outbox_pending", `SELECT COUNT(*) FROM outbox WHERE status='PENDING'`},
		{"avatar_budget_reserved_units", `SELECT COALESCE(SUM(reserved),0) FROM budgets`},
		{"avatar_budget_spent_units", `SELECT COALESCE(SUM(spent),0) FROM budgets`},
	}
	for _, q := range queries {
		var n int64
		if e := a.DB.QueryRow(q.query).Scan(&n); e != nil {
			bad(w, 503, "metrics database unavailable")
			return
		}
		gauges[q.name] = n
	}
	var oldest sql.NullInt64
	if e := a.DB.QueryRow(`SELECT MIN(created) FROM jobs WHERE status IN ('ACCEPTED','PROCESSING','WAITING_DEPENDENCY')`).Scan(&oldest); e != nil {
		bad(w, 503, "metrics database unavailable")
		return
	}
	gauges["avatar_oldest_pending_age_seconds"] = 0
	if oldest.Valid {
		gauges["avatar_oldest_pending_age_seconds"] = max(0, (now()-oldest.Int64)/1000)
	}
	states := []string{"ACCEPTED", "PROCESSING", "WAITING_DEPENDENCY", "NEEDS_RECONCILIATION", "COMPLETED", "PARTIAL_COMPLETED", "REJECTED", "FAILED", "CANCELLED", "EXPIRED", "SUPERSEDED"}
	counts := map[string]int64{}
	rows, e := a.DB.Query(`SELECT status,COUNT(*) FROM jobs GROUP BY status`)
	if e != nil {
		bad(w, 503, "metrics database unavailable")
		return
	}
	for rows.Next() {
		var st string
		var n int64
		if e = rows.Scan(&st, &n); e != nil {
			rows.Close()
			bad(w, 503, "metrics database unavailable")
			return
		}
		counts[st] = n
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		bad(w, 503, "metrics database unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for name, n := range gauges {
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n", name, name, n)
	}
	fmt.Fprintln(w, "# TYPE avatar_jobs_by_status gauge")
	for _, st := range states {
		fmt.Fprintf(w, "avatar_jobs_by_status{status=%q} %d\n", st, counts[st])
	}
}
