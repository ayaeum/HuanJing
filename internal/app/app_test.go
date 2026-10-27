package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTest(t *testing.T) *App {
	t.Helper()
	a, e := Open(filepath.Join(t.TempDir(), "db.sqlite"), 2)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.DB.Close() })
	return a
}
func request(t *testing.T, a *App, method, path, body, key string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	out := map[string]any{}
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w.Code, out
}
func submit(t *testing.T, a *App, fault string) (string, string) {
	t.Helper()
	code, v := request(t, a, "POST", "/api/posts", fmt.Sprintf(`{"text":"徒步旅行自然摄影","fault":%q}`, fault), ID("k"))
	if code != 202 {
		t.Fatal(v)
	}
	return v["post_id"].(string), v["job_id"].(string)
}
func advance(t *testing.T, a *App, id string, stop string) Job {
	t.Helper()
	for i := 0; i < 40; i++ {
		j, e := a.Job(id, "demo")
		if e != nil {
			t.Fatal(e)
		}
		if j.Stage == stop || j.Status == "COMPLETED" || j.Status == "PARTIAL_COMPLETED" || j.Status == "REJECTED" || j.Status == "FAILED" || j.Status == "NEEDS_RECONCILIATION" || j.Status == "CANCELLED" || j.Status == "EXPIRED" {
			return j
		}
		a.DB.Exec(`UPDATE jobs SET next_run=0 WHERE id=?`, id)
		j, e = a.claim()
		if e != nil {
			t.Fatal(e)
		}
		if e = a.step(context.Background(), j); e != nil {
			a.fail(j, e)
		}
	}
	t.Fatal("job did not settle")
	return Job{}
}
func TestSuccessAndIdempotency(t *testing.T) {
	a := newTest(t)
	body := `{"text":"徒步旅行自然摄影"}`
	code, v := request(t, a, "POST", "/api/posts", body, "same")
	if code != 202 {
		t.Fatal(v)
	}
	_, v2 := request(t, a, "POST", "/api/posts", body, "same")
	if v["job_id"] != v2["job_id"] {
		t.Fatal("duplicate job")
	}
	code, _ = request(t, a, "POST", "/api/posts", `{"text":"different"}`, "same")
	if code != 409 {
		t.Fatal("missing conflict")
	}
	j := advance(t, a, v["job_id"].(string), "")
	if j.Status != "COMPLETED" {
		t.Fatalf("%+v", j)
	}
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND avatar_id='travel'`, j.PostID).Scan(&n)
	if n != 2 {
		t.Fatalf("expected independent like/comment: %d", n)
	}
	_, detail := request(t, a, "GET", "/api/jobs/"+j.ID, "", "")
	if strings.Contains(js(detail), "PRIVATE_CANARY") {
		t.Fatal("private context leaked")
	}
}
func TestRetryAndUnknownPartialSuccess(t *testing.T) {
	a := newTest(t)
	_, id := submit(t, a, "retry_once")
	j := advance(t, a, id, "")
	if j.Status != "COMPLETED" {
		t.Fatalf("retry: %+v", j)
	}
	post, id := submit(t, a, "unknown_once")
	j = advance(t, a, id, "")
	if j.Status != "NEEDS_RECONCILIATION" {
		t.Fatalf("unknown: %+v", j)
	}
	var likes, comments int
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND kind='LIKE'`, post).Scan(&likes)
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND kind='COMMENT'`, post).Scan(&comments)
	if likes < 1 || comments != 0 {
		t.Fatal("partial actions lost")
	}
	code, v := request(t, a, "POST", "/api/jobs/"+id+"/reconcile", "", "")
	if code != 202 {
		t.Fatal(v)
	}
	j = advance(t, a, id, "")
	if j.Status != "COMPLETED" {
		t.Fatalf("reconcile: %+v", j)
	}
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND avatar_id='travel'`, post).Scan(&comments)
	if comments != 2 {
		t.Fatal("duplicate or lost actions")
	}
}
func TestDeleteAndUnknownAccounting(t *testing.T) {
	a := newTest(t)
	post, id := submit(t, a, "unknown_once")
	advance(t, a, id, "")
	code, _ := request(t, a, "DELETE", "/api/posts/"+post, "", "")
	if code != 200 {
		t.Fatal(code)
	}
	code, v := request(t, a, "POST", "/api/jobs/"+id+"/reconcile", "", "")
	if code != 202 {
		t.Fatal(v)
	}
	j, _ := a.Job(id, "demo")
	if j.Status != "CANCELLED" {
		t.Fatal("reconciliation revived deleted task")
	}
	var reserved int
	a.DB.QueryRow(`SELECT reserved FROM budgets WHERE tenant='demo'`).Scan(&reserved)
	if reserved != 0 {
		t.Fatal("unknown reservation stuck")
	}
	code, _ = request(t, a, "GET", "/api/posts/"+post, "", "")
	if code != 410 {
		t.Fatal("deleted visible")
	}
}
func TestEpochAndMemoryRevocation(t *testing.T) {
	a := newTest(t)
	_, id := submit(t, a, "")
	advance(t, a, id, "PUBLISH")
	j, e := a.claim()
	if e != nil {
		t.Fatal(e)
	}
	cs, _ := a.candidates(id)
	var travel Candidate
	for _, c := range cs {
		if c.AvatarID == "travel" {
			travel = c
		}
	}
	code, v := request(t, a, "DELETE", "/api/memories/demo-travel1", "", "")
	if code != 200 {
		t.Fatal(v)
	}
	ok, e := a.commitAction(j, travel, "LIKE", "")
	if e != nil || ok {
		t.Fatalf("revoked memory allowed: %v %v", ok, e)
	}
	a.DB.Exec(`UPDATE jobs SET epoch=epoch+1 WHERE id=?`, id)
	if e = a.saveStage(j, "DONE", nil); e == nil {
		t.Fatal("old epoch accepted")
	}
}
func TestBudgetExpiryAndTenantIsolation(t *testing.T) {
	a := newTest(t)
	request(t, a, "POST", "/api/budget", `{"limit":0}`, "")
	_, id := submit(t, a, "")
	j := advance(t, a, id, "")
	if j.Status != "FAILED" {
		t.Fatalf("budget: %+v", j)
	}
	var spent, res int
	a.DB.QueryRow(`SELECT spent,reserved FROM budgets WHERE tenant='demo'`).Scan(&spent, &res)
	if spent+res != 0 {
		t.Fatal("exceeded zero budget")
	}
	_, id = submit(t, a, "")
	a.DB.Exec(`UPDATE jobs SET deadline=0 WHERE id=?`, id)
	a.claim()
	j, _ = a.Job(id, "demo")
	if j.Status != "EXPIRED" {
		t.Fatal("expiry rolled back")
	}
	r := httptest.NewRequest("GET", "/api/jobs/"+id, nil)
	r.Header.Set("X-Tenant-ID", "other")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("tenant leak")
	}
}
func TestEditFreezesActionsAndFencesOldJob(t *testing.T) {
	a := newTest(t)
	post, id := submit(t, a, "")
	advance(t, a, id, "")
	code, v := request(t, a, "PATCH", "/api/posts/"+post, `{"expected_version":1,"text":"旅行徒步自然摄影新版本"}`, "")
	if code != 202 {
		t.Fatal(v)
	}
	j := advance(t, a, v["job_id"].(string), "")
	if j.Status != "COMPLETED" {
		t.Fatalf("edit: %+v", j)
	}
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND avatar_id='travel'`, post).Scan(&n)
	if n != 2 {
		t.Fatal("edit duplicated actions")
	}
}

func TestCancelledRunningReceiptReconcilesWithoutRestart(t *testing.T) {
	a := newTest(t)
	post, id := submit(t, a, "")
	_, e := a.DB.Exec(`INSERT INTO operations VALUES('crash-receipt','demo',?,'','understand',1,'RUNNING',1,'','','durable mock result',?)`, id, now())
	if e != nil {
		t.Fatal(e)
	}
	a.DB.Exec(`UPDATE budgets SET reserved=1 WHERE tenant='demo'`)
	request(t, a, "DELETE", "/api/posts/"+post, "", "")
	code, v := request(t, a, "POST", "/api/jobs/"+id+"/reconcile", "", "")
	if code != 202 {
		t.Fatal(v)
	}
	j, _ := a.Job(id, "demo")
	if j.Status != "CANCELLED" {
		t.Fatal("cancelled job resumed")
	}
	var reserved, spent int
	a.DB.QueryRow(`SELECT reserved,spent FROM budgets WHERE tenant='demo'`).Scan(&reserved, &spent)
	if reserved != 0 || spent != 1 {
		t.Fatalf("receipt accounting: reserved=%d spent=%d", reserved, spent)
	}
}
func TestDecisionReceiptKeepsOriginalMemorySnapshot(t *testing.T) {
	a := newTest(t)
	_, id := submit(t, a, "")
	advance(t, a, id, "DECIDE")
	j, e := a.claim()
	if e != nil {
		t.Fatal(e)
	}
	snapshot := `{"actions":["LIKE","COMMENT"],"sources":["demo-travel1"],"reason":"SHARED_INTEREST"}`
	_, e = a.DB.Exec(`INSERT INTO operations VALUES('decision-receipt','demo',?,'travel','decision',1,'SUCCEEDED',1,?,'',?,?)`, id, snapshot, snapshot, now())
	if e != nil {
		t.Fatal(e)
	}
	request(t, a, "DELETE", "/api/memories/demo-travel1", "", "")
	if e = a.step(context.Background(), j); e != nil {
		t.Fatal(e)
	}
	advance(t, a, id, "")
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE avatar_id='travel'`).Scan(&n)
	if n != 0 {
		t.Fatal("cached old decision was rebound to new sources")
	}
}
func TestSourceDeletionFencesDerivedMemoryAndOutbox(t *testing.T) {
	a := newTest(t)
	source, sid := submit(t, a, "")
	advance(t, a, sid, "")
	for a.projectEvent() {
	}
	_, target := submit(t, a, "")
	advance(t, a, target, "PUBLISH")
	request(t, a, "DELETE", "/api/posts/"+source, "", "")
	advance(t, a, target, "")
	var n int
	a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=(SELECT post_id FROM jobs WHERE id=?) AND avatar_id='travel'`, target).Scan(&n)
	if n != 0 {
		t.Fatal("deleted source did not fence target")
	}
	var revoked int
	a.DB.QueryRow(`SELECT COUNT(*) FROM memories WHERE source_post=? AND revoked=0`, source).Scan(&revoked)
	if revoked != 0 {
		t.Fatal("source memory remained active")
	}
}

func TestCandidateSnapshotSurvivesStageCrash(t *testing.T) {
	a := newTest(t)
	_, id := submit(t, a, "")
	advance(t, a, id, "MATCH")
	j, e := a.claim()
	if e != nil {
		t.Fatal(e)
	}
	if e = a.match(j); e != nil {
		t.Fatal(e)
	}
	before, _ := a.candidates(id)
	a.DB.Exec(`UPDATE avatars SET active=0 WHERE tenant='demo'`)
	a.DB.Exec(`INSERT INTO avatars VALUES('new','demo','new','旅行','旅行',1,1)`)
	if e = a.match(j); e != nil {
		t.Fatal(e)
	}
	after, _ := a.candidates(id)
	if len(before) != len(after) {
		t.Fatal("candidate snapshot changed after restart")
	}
	for i := range before {
		if before[i].AvatarID != after[i].AvatarID {
			t.Fatal("candidate snapshot replaced")
		}
	}
}
func TestAllStaleIsFailedNotPartial(t *testing.T) {
	a := newTest(t)
	_, id := submit(t, a, "")
	advance(t, a, id, "PUBLISH")
	a.DB.Exec(`UPDATE avatars SET active=0,epoch=epoch+1 WHERE tenant='demo'`)
	j := advance(t, a, id, "")
	if j.Status != "FAILED" {
		t.Fatalf("all stale should fail: %+v", j)
	}
}

func TestFourMockActionPlans(t *testing.T) {
	for _, tc := range []struct {
		text            string
		likes, comments int
	}{{"徒步旅行摄影，只评论", 0, 1}, {"徒步旅行摄影，只点赞", 1, 0}, {"徒步旅行摄影，不需要互动", 0, 0}, {"徒步旅行摄影", 1, 1}} {
		t.Run(tc.text, func(t *testing.T) {
			a := newTest(t)
			code, v := request(t, a, "POST", "/api/posts", js(map[string]any{"text": tc.text, "max_candidates": 1}), ID("k"))
			if code != 202 {
				t.Fatal(v)
			}
			j := advance(t, a, v["job_id"].(string), "")
			if j.Status != "COMPLETED" {
				t.Fatalf("%+v", j)
			}
			var likes, comments int
			a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND kind='LIKE'`, j.PostID).Scan(&likes)
			a.DB.QueryRow(`SELECT COUNT(*) FROM interactions WHERE post_id=? AND kind='COMMENT'`, j.PostID).Scan(&comments)
			if likes != tc.likes || comments != tc.comments {
				t.Fatalf("got LIKE=%d COMMENT=%d", likes, comments)
			}
		})
	}
}

func TestHealthReportsUnavailableDatabase(t *testing.T) {
	a := newTest(t)
	a.DB.Close()
	code, _ := request(t, a, "GET", "/api/health", "", "")
	if code != 503 {
		t.Fatalf("health %d after DB close", code)
	}
}
