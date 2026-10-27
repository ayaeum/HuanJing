package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

type App struct {
	DB        *sql.DB
	Workers   int
	UploadDir string
	WebDir    string
	Active    atomic.Int64
}

func ID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func now() int64      { return time.Now().UnixMilli() }
func js(v any) string { b, _ := json.Marshal(v); return string(b) }
func Open(path string, workers int) (*App, error) {
	if workers < 1 || workers > 32 {
		return nil, fmt.Errorf("WORKERS must be 1..32")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	a := &App{DB: db, Workers: workers, UploadDir: filepath.Join(filepath.Dir(path), "uploads"), WebDir: "web"}
	if err = os.MkdirAll(a.UploadDir, 0700); err != nil {
		return nil, err
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS posts(id TEXT PRIMARY KEY,tenant TEXT NOT NULL,text TEXT NOT NULL,version INTEGER DEFAULT 1,deleted INTEGER DEFAULT 0,attachments TEXT NOT NULL,created INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,tenant TEXT NOT NULL,post_id TEXT NOT NULL,source_version INTEGER DEFAULT 1,status TEXT NOT NULL,stage TEXT NOT NULL,attempts INTEGER DEFAULT 0,failures INTEGER DEFAULT 0,epoch INTEGER DEFAULT 0,lease INTEGER DEFAULT 0,next_run INTEGER DEFAULT 0,deadline INTEGER NOT NULL,max_candidates INTEGER NOT NULL,fault TEXT NOT NULL,error TEXT DEFAULT '',created INTEGER NOT NULL,updated INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS jobs_due ON jobs(status,next_run,lease);
 CREATE TABLE IF NOT EXISTS request_keys(tenant TEXT,key TEXT,hash TEXT,post_id TEXT,job_id TEXT,PRIMARY KEY(tenant,key));
 CREATE TABLE IF NOT EXISTS stages(job_id TEXT,name TEXT,data TEXT,created INTEGER,PRIMARY KEY(job_id,name));
 CREATE TABLE IF NOT EXISTS avatars(id TEXT,tenant TEXT,name TEXT,persona TEXT,interests TEXT,active INTEGER DEFAULT 1,epoch INTEGER DEFAULT 1,PRIMARY KEY(tenant,id));
 CREATE TABLE IF NOT EXISTS memories(id TEXT PRIMARY KEY,tenant TEXT,avatar_id TEXT,content TEXT,public INTEGER,source_post TEXT DEFAULT '',source_interaction TEXT DEFAULT '',revoked INTEGER DEFAULT 0,UNIQUE(tenant,source_interaction,avatar_id));
 CREATE TABLE IF NOT EXISTS candidates(job_id TEXT,avatar_id TEXT,name TEXT,score REAL,evidence TEXT,actions TEXT,status TEXT,reason TEXT,comment TEXT DEFAULT '',avatar_epoch INTEGER DEFAULT 0,sources TEXT DEFAULT '[]',PRIMARY KEY(job_id,avatar_id));
 CREATE TABLE IF NOT EXISTS operations(operation_key TEXT PRIMARY KEY,tenant TEXT,job_id TEXT,avatar_id TEXT,name TEXT,attempt INTEGER,status TEXT,cost INTEGER,result TEXT,error TEXT,provider_result TEXT,created INTEGER);
 CREATE TABLE IF NOT EXISTS budgets(tenant TEXT PRIMARY KEY,limit_units INTEGER DEFAULT 10000,spent INTEGER DEFAULT 0,reserved INTEGER DEFAULT 0);
 CREATE TABLE IF NOT EXISTS decisions(tenant TEXT,post_id TEXT,avatar_id TEXT,actions TEXT,PRIMARY KEY(tenant,post_id,avatar_id));
 CREATE TABLE IF NOT EXISTS action_states(job_id TEXT,avatar_id TEXT,kind TEXT,status TEXT,error TEXT DEFAULT '',interaction_id TEXT DEFAULT '',PRIMARY KEY(job_id,avatar_id,kind));
 CREATE TABLE IF NOT EXISTS interactions(id TEXT PRIMARY KEY,tenant TEXT,post_id TEXT,avatar_id TEXT,kind TEXT,content TEXT,created INTEGER,UNIQUE(tenant,post_id,avatar_id,kind));
 CREATE TABLE IF NOT EXISTS outbox(id TEXT PRIMARY KEY,tenant TEXT,post_id TEXT,interaction_id TEXT,avatar_id TEXT,content TEXT,status TEXT DEFAULT 'PENDING',created INTEGER);
 CREATE TABLE IF NOT EXISTS uploads(id TEXT PRIMARY KEY,tenant TEXT,name TEXT,mime TEXT,size INTEGER,caption TEXT,path TEXT,created INTEGER);
 CREATE TABLE IF NOT EXISTS upload_bindings(tenant TEXT,upload_id TEXT,post_id TEXT,PRIMARY KEY(tenant,upload_id,post_id));
 INSERT OR IGNORE INTO upload_bindings SELECT p.tenant,a.value,p.id FROM posts p,json_each(p.attachments) a;
 CREATE TRIGGER IF NOT EXISTS post_upload_insert AFTER INSERT ON posts BEGIN
 INSERT OR IGNORE INTO upload_bindings SELECT NEW.tenant,value,NEW.id FROM json_each(NEW.attachments); END;
 CREATE TRIGGER IF NOT EXISTS post_upload_update AFTER UPDATE OF attachments ON posts BEGIN
 INSERT OR IGNORE INTO upload_bindings SELECT NEW.tenant,value,NEW.id FROM json_each(NEW.attachments); END;
 `)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = a.Seed("demo"); err != nil {
		return nil, err
	}
	return a, nil
}
func (a *App) Seed(tenant string) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO budgets(tenant) VALUES(?)`, tenant); err != nil {
		return err
	}
	seeds := [][4]string{{"travel", "山野旅行家", "喜欢徒步、旅行、自然与摄影，语气温和好奇", "旅行 徒步 山野 自然 摄影 hiking travel"}, {"fitness", "运动搭子", "喜欢跑步、健身和自我成长，重视安全与鼓励", "跑步 健身 运动 马拉松 fitness running"}, {"food", "厨房朋友", "喜欢烹饪和咖啡，乐于交流简单食谱", "做饭 美食 烹饪 咖啡 面包 food cooking"}, {"tech", "技术同伴", "喜欢编程、AI和开源，表达具体审慎", "编程 技术 AI 开源 Go 系统 programming code"}, {"art", "艺术观察员", "喜欢绘画、音乐、电影与日常创作", "绘画 音乐 电影 艺术 创作 music art"}}
	for _, s := range seeds {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO avatars(id,tenant,name,persona,interests) VALUES(?,?,?,?,?)`, s[0], tenant, s[1], s[2], s[3]); err != nil {
			return err
		}
		for i, m := range []string{"我喜欢和他人分享关于" + s[3] + "的经历。", "PRIVATE_CANARY_" + s[0] + "：私人经历，仅用于内部判断，喜欢真诚的互动。"} {
			_, err = tx.Exec(`INSERT OR IGNORE INTO memories(id,tenant,avatar_id,content,public,source_interaction) VALUES(?,?,?,?,?,?)`, tenant+"-"+s[0]+fmt.Sprint(i), tenant, s[0], m, 1-i, "seed-"+s[0]+fmt.Sprint(i))
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type Job struct {
	ID       string `json:"id"`
	Tenant   string `json:"-"`
	PostID   string `json:"post_id"`
	Status   string `json:"status"`
	Stage    string `json:"stage"`
	Attempts int    `json:"attempts"`
	Failures int    `json:"-"`
	Epoch    int    `json:"epoch"`
	Lease    int64  `json:"-"`
	Next     int64  `json:"next_run"`
	Deadline int64  `json:"deadline"`
	Max      int    `json:"max_candidates"`
	Fault    string `json:"fault"`
	Error    string `json:"error"`
	Created  int64  `json:"created_at"`
	Updated  int64  `json:"updated_at"`
}

const jobcols = "id,tenant,post_id,status,stage,attempts,failures,epoch,lease,next_run,deadline,max_candidates,fault,error,created,updated"

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (j Job, e error) {
	e = r.Scan(&j.ID, &j.Tenant, &j.PostID, &j.Status, &j.Stage, &j.Attempts, &j.Failures, &j.Epoch, &j.Lease, &j.Next, &j.Deadline, &j.Max, &j.Fault, &j.Error, &j.Created, &j.Updated)
	return
}
func (a *App) Job(id, tenant string) (Job, error) {
	return scanJob(a.DB.QueryRow("SELECT "+jobcols+" FROM jobs WHERE id=? AND tenant=?", id, tenant))
}
func valid(tx *sql.Tx, j Job) error {
	var status string
	var ep int
	var deadline int64
	var deleted int
	err := tx.QueryRow(`SELECT j.status,j.epoch,j.deadline,CASE WHEN p.version<>j.source_version THEN 1 ELSE p.deleted END FROM jobs j JOIN posts p ON p.id=j.post_id WHERE j.id=? AND j.tenant=?`, j.ID, j.Tenant).Scan(&status, &ep, &deadline, &deleted)
	if err != nil {
		return err
	}
	if ep != j.Epoch || status != "PROCESSING" || deleted != 0 || deadline <= now() {
		return fmt.Errorf("stale or expired execution")
	}
	return nil
}
func (a *App) Start(ctx context.Context) {
	for i := 0; i < a.Workers; i++ {
		go a.worker(ctx)
	}
	go a.events(ctx)
}
