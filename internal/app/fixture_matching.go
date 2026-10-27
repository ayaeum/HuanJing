package app

// Deterministic fixtures support offline workflow regression only. Production
// startup defaults to real semantics and never falls back to this implementation.
import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var vocabulary = [][]string{{"旅行", "徒步", "自然", "山", "摄影", "travel", "hiking"}, {"跑步", "健身", "运动", "马拉松", "fitness", "running"}, {"做饭", "美食", "烹饪", "咖啡", "面包", "food", "cooking"}, {"编程", "技术", "ai", "开源", "go", "系统", "programming", "code"}, {"绘画", "音乐", "电影", "艺术", "创作", "music", "art"}}

// topicWord avoids substring matches such as "go" in "going" or "art" in "start".
func topicWord(s, word string) bool {
	s = strings.ToLower(s)
	for offset := 0; offset < len(s); {
		pos := strings.Index(s[offset:], word)
		if pos < 0 {
			return false
		}
		pos += offset
		end := pos + len(word)
		ascii := word[0] < 128
		left, right := true, true
		if ascii && pos > 0 {
			r, _ := utf8.DecodeLastRuneInString(s[:pos])
			left = !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}
		if ascii && end < len(s) {
			r, _ := utf8.DecodeRuneInString(s[end:])
			right = !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}
		if left && right {
			return true
		}
		offset = end
	}
	return false
}
func fixtureTopics(s string) []string {
	out := []string{}
	for _, group := range vocabulary {
		for _, w := range group {
			if topicWord(s, w) {
				out = append(out, w)
			}
		}
	}
	return out
}
func sharedTopics(text, interests string) []string {
	out := []string{}
	for _, group := range vocabulary {
		contentHits, profileHit := []string{}, false
		for _, w := range group {
			if topicWord(text, w) {
				contentHits = append(contentHits, w)
			}
			if topicWord(interests, w) {
				profileHit = true
			}
		}
		if profileHit {
			out = append(out, contentHits...)
		}
	}
	return out
}
func fixtureEmbed(s string) [64]float64 {
	var v [64]float64
	rs := []rune(strings.ToLower(s))
	for i := 0; i+1 < len(rs); i++ {
		if unicode.IsSpace(rs[i]) {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(string(rs[i : i+2])))
		v[h.Sum32()%64]++
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range v {
			v[i] /= norm
		}
	}
	return v
}
func cosine(a, b [64]float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
func (a *App) matchFixture(j Job) error {
	if a.AIMode != "TEST_FIXTURE" {
		return &flowError{"FAILED", "deterministic matching is restricted to explicit test_fixture mode"}
	}
	var frozen int
	err := a.DB.QueryRow(`SELECT 1 FROM stages WHERE job_id=? AND name='MATCH'`, j.ID).Scan(&frozen)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	text, e := a.content(j)
	if e != nil {
		return e
	}
	rows, e := a.DB.Query(`SELECT id,name,interests,epoch FROM avatars WHERE tenant=? AND active=1`, j.Tenant)
	if e != nil {
		return e
	}
	type item struct {
		c        Candidate
		sem      float64
		hits     []string
		relation int
	}
	items := []item{}
	for rows.Next() {
		var it item
		var interest string
		if e = rows.Scan(&it.c.AvatarID, &it.c.Name, &interest, &it.c.Epoch); e != nil {
			rows.Close()
			return e
		}
		it.sem = cosine(fixtureEmbed(text), fixtureEmbed(interest))
		it.hits = sharedTopics(text, interest)
		// Hash similarity and relationship history rank qualified candidates only.
		// The mock has no semantic model that can justify unrelated interests.
		if len(it.hits) == 0 {
			continue
		}

		it.c.Actions = []string{}
		it.c.Sources = []string{}
		it.c.Evidence = []string{}
		it.c.Status = "PENDING"
		items = append(items, it)
	}
	rows.Close()
	for i := range items {
		if e = a.DB.QueryRow(`SELECT COUNT(*) FROM interactions i JOIN posts p ON p.id=i.post_id WHERE i.tenant=? AND i.avatar_id=? AND p.deleted=0`, j.Tenant, items[i].c.AvatarID).Scan(&items[i].relation); e != nil {
			return e
		}
	}
	// Independent ranked channels; raw cosine, keyword counts and history counts
	// never share a score scale. Weighted reciprocal rank fusion uses k=60.
	for _, lane := range []string{"semantic", "keyword", "public_history"} {
		indices := []int{}
		for i, it := range items {
			if (lane == "semantic" && it.sem >= .20) || (lane == "keyword" && len(it.hits) > 0) || (lane == "public_history" && it.relation > 0) {
				indices = append(indices, i)
			}
		}
		sort.Slice(indices, func(x, y int) bool {
			a, b := items[indices[x]], items[indices[y]]
			switch lane {
			case "keyword":
				if len(a.hits) != len(b.hits) {
					return len(a.hits) > len(b.hits)
				}
			case "semantic":
				if a.sem != b.sem {
					return a.sem > b.sem
				}
			case "public_history":
				if a.relation != b.relation {
					return a.relation > b.relation
				}
			}
			return a.c.AvatarID < b.c.AvatarID
		})
		for rank, i := range indices {
			weight := 1.0
			if lane == "keyword" {
				weight = 2
			}
			items[i].c.Score += weight / float64(61+rank)
			evidence := fmt.Sprintf("%s rank=%d rrf=%.5f", lane, rank+1, weight/float64(61+rank))
			if lane == "keyword" {
				evidence += " matches=" + strings.Join(items[i].hits, ",")
			}
			if lane == "semantic" {
				evidence += fmt.Sprintf(" cosine=%.3f", items[i].sem)
			}
			items[i].c.Evidence = append(items[i].c.Evidence, evidence)
		}
	}
	cs := []Candidate{}
	for _, it := range items {
		if it.c.Score > 0 {
			cs = append(cs, it.c)
		}
	}
	sort.Slice(cs, func(i, k int) bool {
		if cs[i].Score == cs[k].Score {
			return cs[i].AvatarID < cs[k].AvatarID
		}
		return cs[i].Score > cs[k].Score
	})
	if len(cs) > j.Max {
		cs = cs[:j.Max]
	}
	tx, e := a.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = valid(tx, j); e != nil {
		return e
	}
	for _, c := range cs {
		_, e = tx.Exec(`INSERT OR IGNORE INTO candidates(job_id,avatar_id,name,score,evidence,actions,status,reason,avatar_epoch) VALUES(?,?,?,?,?,?,?,?,?)`, j.ID, c.AvatarID, c.Name, c.Score, js(c.Evidence), "[]", c.Status, "", c.Epoch)
		if e != nil {
			return e
		}
	}
	if _, e = tx.Exec(`INSERT OR IGNORE INTO stages(job_id,name,data,created) VALUES(?,'MATCH',?,?)`, j.ID, js(map[string]any{"strategy": "topic-qualified weighted RRF hash similarity/keyword/public_history", "candidate_count": len(cs), "frozen": true}), now()); e != nil {
		return e
	}
	return tx.Commit()
}
