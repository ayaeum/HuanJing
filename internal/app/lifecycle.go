package app

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (a *App) editPost(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Expected    int      `json:"expected_version"`
		Text        string   `json:"text"`
		Attachments []string `json:"attachment_ids"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Text = strings.TrimSpace(b.Text)
	if b.Expected < 1 || len(b.Text) > 16000 || len(b.Attachments) > 4 || (b.Text == "" && len(b.Attachments) == 0) {
		bad(w, 400, "expected_version and content required")
		return
	}
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	var version, deleted int
	e = tx.QueryRow(`SELECT version,deleted FROM posts WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r)).Scan(&version, &deleted)
	if e != nil {
		bad(w, 404, "post not found")
		return
	}
	if deleted != 0 {
		bad(w, 410, "post deleted")
		return
	}
	if version != b.Expected {
		bad(w, 409, "version conflict")
		return
	}
	for _, id := range b.Attachments {
		var n int
		if tx.QueryRow(`SELECT 1 FROM uploads WHERE id=? AND tenant=?`, id, tenant(r)).Scan(&n) != nil {
			bad(w, 400, "unauthorized attachment")
			return
		}
	}
	_, e = tx.Exec(`UPDATE posts SET text=?,attachments=?,version=version+1 WHERE id=?`, b.Text, js(b.Attachments), r.PathValue("id"))
	if e == nil {
		_, e = tx.Exec(`UPDATE jobs SET status='SUPERSEDED',epoch=epoch+1,lease=0,updated=? WHERE post_id=? AND status NOT IN ('COMPLETED','PARTIAL_COMPLETED','REJECTED','CANCELLED')`, now(), r.PathValue("id"))
	}
	jid := ID("j_")
	if e == nil {
		_, e = tx.Exec(`INSERT INTO jobs(id,tenant,post_id,source_version,status,stage,deadline,max_candidates,fault,created,updated) VALUES(?,?,?,?,'ACCEPTED','MODERATE',?,4,'',?,?)`, jid, tenant(r), r.PathValue("id"), version+1, now()+120000, now(), now())
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	respond(w, 202, map[string]any{"post_id": r.PathValue("id"), "job_id": jid, "version": version + 1, "status": "ACCEPTED"})
}
func (a *App) memories(w http.ResponseWriter, r *http.Request) {
	rs, e := a.DB.Query(`SELECT id,avatar_id,content,public,source_post,revoked FROM memories WHERE tenant=? ORDER BY id LIMIT 100`, tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer rs.Close()
	out := []map[string]any{}
	for rs.Next() {
		var id, av, c, source string
		var pub, rev int
		if rs.Scan(&id, &av, &c, &pub, &source, &rev) != nil {
			continue
		}
		if pub == 0 {
			c = "[PRIVATE: hidden from API]"
		}
		if rev != 0 {
			c = "[REVOKED]"
		}
		out = append(out, map[string]any{"id": id, "avatar_id": av, "content": c, "public": pub == 1, "source_post": source, "revoked": rev == 1})
	}
	respond(w, 200, map[string]any{"memories": out})
}
func (a *App) revokeMemory(w http.ResponseWriter, r *http.Request) {
	tx, e := a.DB.Begin()
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec(`UPDATE memories SET revoked=1 WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r))
	if e != nil {
		bad(w, 500, e.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		bad(w, 404, "memory not found")
		return
	}
	if e = tx.Commit(); e != nil {
		bad(w, 500, e.Error())
		return
	}
	respond(w, 200, map[string]string{"status": "REVOKED"})
}

// DecodePlan exposes no model rationale. The fixed action set survives edits.
func DecodePlan(s string) []string { var a []string; json.Unmarshal([]byte(s), &a); return a }
