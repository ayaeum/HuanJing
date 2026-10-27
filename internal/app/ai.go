package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// AIConfig contains process-local credentials. It is never serialized or logged.
type AIConfig struct {
	baseURL, apiKey, model string
	timeout                time.Duration
	threshold              float64
	client                 *http.Client
}

func (a *App) ConfigureAIFromEnv() error {
	mode := os.Getenv("AI_MODE")
	if mode == "test_fixture" {
		a.AIMode = "TEST_FIXTURE"
		return nil
	}
	if mode != "" && mode != "real" {
		return errors.New("AI_MODE must be real or test_fixture")
	}
	c := &AIConfig{baseURL: strings.TrimRight(os.Getenv("AI_BASE_URL"), "/"), apiKey: os.Getenv("AI_API_KEY"), model: os.Getenv("AI_MODEL"), timeout: 45 * time.Second, threshold: .65}
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("AI_BASE_URL must be an API base URL without credentials, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return errors.New("AI_BASE_URL requires HTTPS (loopback test servers may use HTTP)")
	}
	if strings.TrimSpace(c.apiKey) == "" || strings.TrimSpace(c.model) == "" {
		return errors.New("real semantics requires AI_BASE_URL, AI_API_KEY and AI_MODEL")
	}
	if value := os.Getenv("AI_TIMEOUT_SECONDS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 120 {
			return errors.New("AI_TIMEOUT_SECONDS must be 1..120")
		}
		c.timeout = time.Duration(n) * time.Second
	}
	if value := os.Getenv("AI_RELEVANCE_THRESHOLD"); value != "" {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || !(n > 0 && n <= 1) {
			return errors.New("AI_RELEVANCE_THRESHOLD must be >0 and <=1")
		}
		c.threshold = n
	}
	c.client = &http.Client{Timeout: c.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a.AI = c
	a.AIMode = "REAL_SEMANTICS"
	return nil
}

type aiReceipt struct {
	Content       string          `json:"content"`
	Model         string          `json:"model"`
	ResponseID    string          `json:"response_id"`
	InputTokens   int64           `json:"input_tokens"`
	OutputTokens  int64           `json:"output_tokens"`
	UsageReported bool            `json:"usage_reported"`
	Request       json.RawMessage `json:"-"`
}

// aiCall persists the request before sending it. A lost reply is not a receipt:
// ambiguous outcomes retain the reservation and require an explicit resolution.
// A budget unit caps requests, not currency; real token usage is recorded separately.
func (a *App) aiCall(ctx context.Context, j Job, name, system string, input any, maxTokens int, validate func(string, json.RawMessage) error) (aiReceipt, error) {
	var out aiReceipt
	if a.AI == nil {
		return out, &flowError{"FAILED", "real AI provider is not configured"}
	}
	requestJSON := js(input)
	tx, err := a.DB.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = valid(tx, j); err != nil {
		return out, err
	}
	var key, status, result, storedRequest, model string
	var attempt int
	err = tx.QueryRow(`SELECT operation_key,status,result,request_json,model,attempt FROM ai_operations WHERE job_id=? AND name=? ORDER BY attempt DESC LIMIT 1`, j.ID, name).Scan(&key, &status, &result, &storedRequest, &model, &attempt)
	if err == nil {
		switch status {
		case "SUCCEEDED":
			if err = json.Unmarshal([]byte(result), &out); err != nil {
				return out, err
			}
			out.Request = json.RawMessage(storedRequest)
			return out, nil
		case "RUNNING", "UNKNOWN":
			if _, err = tx.Exec(`UPDATE ai_operations SET status='UNKNOWN',error='provider outcome unknown; automatic resend blocked',updated=? WHERE operation_key=? AND status='RUNNING'`, now(), key); err != nil {
				return out, err
			}
			if err = tx.Commit(); err != nil {
				return out, err
			}
			return out, &flowError{"NEEDS_RECONCILIATION", "real AI outcome unknown; explicitly abandon the uncertain attempt before authorizing another request"}
		case "INVALID_RESPONSE", "REJECTED":
			return out, &flowError{"FAILED", "AI response or configuration rejected; submit a new post after correcting the cause"}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	attempt++
	key = fmt.Sprintf("%s:%s:%d", j.ID, name, attempt)
	q, err := tx.Exec(`UPDATE budgets SET reserved=reserved+1 WHERE tenant=? AND spent+reserved+1<=limit_units`, j.Tenant)
	if err != nil {
		return out, err
	}
	n, _ := q.RowsAffected()
	if n == 0 {
		return out, &flowError{"WAITING_DEPENDENCY", "request budget exhausted"}
	}
	payload := js(map[string]any{"model": a.AI.model, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": requestJSON}}, "max_tokens": maxTokens})
	hash := sha256.Sum256([]byte(payload))
	_, err = tx.Exec(`INSERT INTO ai_operations(operation_key,tenant,job_id,name,attempt,status,request_json,request_hash,model,created,updated) VALUES(?,?,?,?,?,'RUNNING',?,?,?,?,?)`, key, j.Tenant, j.ID, name, attempt, requestJSON, hex.EncodeToString(hash[:]), a.AI.model, now(), now())
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}

	callCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(j.Deadline))
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.AI.baseURL+"/chat/completions", strings.NewReader(payload))
	if err != nil {
		return out, a.finishAI(key, "REJECTED", out, "invalid AI request configuration")
	}
	req.Header.Set("Authorization", "Bearer "+a.AI.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.AI.client.Do(req)
	if err != nil {
		// Transport errors can follow upstream acceptance. Never log the request
		// URL, transport error or provider body: they may contain credentials.
		return out, a.finishAI(key, "UNKNOWN", out, "AI transport failed or timed out; upstream outcome unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		status = "REJECTED"
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			status = "FAILED"
		}
		return out, a.finishAI(key, status, out, fmt.Sprintf("AI provider HTTP %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return out, a.finishAI(key, "UNKNOWN", out, "AI response interrupted; upstream outcome unknown")
	}
	if len(raw) > 1<<20 {
		return out, a.finishAI(key, "INVALID_RESPONSE", out, "AI response exceeds size limit")
	}
	if bytes.Contains(raw, []byte(a.AI.apiKey)) || bytes.Contains(raw, []byte(a.AI.baseURL)) {
		return out, a.finishAI(key, "INVALID_RESPONSE", out, "AI provider returned sensitive transport configuration")
	}
	var envelope struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			Input  int64 `json:"prompt_tokens"`
			Output int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Choices) != 1 || envelope.Choices[0].FinishReason != "stop" {
		return out, a.finishAI(key, "INVALID_RESPONSE", out, "AI response is incomplete or has an invalid envelope")
	}
	out = aiReceipt{Content: envelope.Choices[0].Message.Content, Model: a.AI.model, ResponseID: envelope.ID, Request: json.RawMessage(requestJSON)}
	if envelope.Usage != nil && envelope.Usage.Input >= 0 && envelope.Usage.Output >= 0 {
		out.InputTokens = envelope.Usage.Input
		out.OutputTokens = envelope.Usage.Output
		out.UsageReported = true
	}
	if err = validate(out.Content, out.Request); err != nil {
		out.Content = "" // malformed or injected output is not rendered or persisted
		return out, a.finishAI(key, "INVALID_RESPONSE", out, "AI structured output failed schema or source validation")
	}
	err = a.finishAI(key, "SUCCEEDED", out, "")
	return out, err
}

func (a *App) finishAI(key, status string, out aiReceipt, message string) error {
	tx, err := a.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tenant string
	if err = tx.QueryRow(`SELECT tenant FROM ai_operations WHERE operation_key=?`, key).Scan(&tenant); err != nil {
		return err
	}
	q, err := tx.Exec(`UPDATE ai_operations SET status=?,result=?,response_id=?,input_tokens=?,output_tokens=?,usage_reported=?,error=?,updated=? WHERE operation_key=? AND status='RUNNING'`, status, js(out), out.ResponseID, out.InputTokens, out.OutputTokens, out.UsageReported, message, now(), key)
	if err != nil {
		return err
	}
	n, _ := q.RowsAffected()
	if n == 0 {
		return &flowError{"NEEDS_RECONCILIATION", "AI attempt ownership changed; late response cannot advance the job"}
	}
	if status != "UNKNOWN" {
		if _, err = tx.Exec(`UPDATE budgets SET reserved=reserved-1,spent=spent+1 WHERE tenant=?`, tenant); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	switch status {
	case "SUCCEEDED":
		return nil
	case "UNKNOWN":
		return &flowError{"NEEDS_RECONCILIATION", message}
	case "FAILED":
		return errors.New(message)
	default:
		return &flowError{"FAILED", message}
	}
}

func strictJSON(content string, dst any) error {
	d := json.NewDecoder(bytes.NewBufferString(content))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (a *App) resolveAI(w http.ResponseWriter, r *http.Request) {
	var b struct {
		OperationKey string `json:"operation_key"`
		Resolution   string `json:"resolution"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Resolution != "abandon_and_charge" {
		bad(w, 400, "resolution must be abandon_and_charge; provider cost remains uncertain")
		return
	}
	tx, err := a.DB.Begin()
	if err != nil {
		bad(w, 500, "database unavailable")
		return
	}
	defer tx.Rollback()
	var status string
	if tx.QueryRow(`SELECT status FROM jobs WHERE id=? AND tenant=?`, r.PathValue("id"), tenant(r)).Scan(&status) != nil {
		bad(w, 404, "job not found")
		return
	}
	q, err := tx.Exec(`UPDATE ai_operations SET status='ABANDONED',error='user explicitly abandoned uncertain attempt; one request unit consumed',updated=? WHERE operation_key=? AND job_id=? AND tenant=? AND (status='UNKNOWN' OR (status='RUNNING' AND ? IN ('CANCELLED','SUPERSEDED','EXPIRED','FAILED')))`, now(), b.OperationKey, r.PathValue("id"), tenant(r), status)
	if err != nil {
		bad(w, 500, "resolution failed")
		return
	}
	n, _ := q.RowsAffected()
	if n == 0 {
		bad(w, 409, "no eligible uncertain AI attempt")
		return
	}
	_, err = tx.Exec(`UPDATE budgets SET reserved=reserved-1,spent=spent+1 WHERE tenant=?`, tenant(r))
	if err == nil {
		_, err = tx.Exec(`UPDATE jobs SET status='FAILED',error='AI attempt abandoned; explicit retry may incur another provider charge',epoch=epoch+1,lease=0,updated=? WHERE id=? AND status='NEEDS_RECONCILIATION'`, now(), r.PathValue("id"))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		bad(w, 500, "resolution failed")
		return
	}
	respond(w, 200, map[string]string{"status": "ABANDONED", "next_step": "explicit retry may make a new billable request; this is not provider reconciliation"})
}

// Only bounded accounting metadata is exposed, including after source deletion.
// Raw input, provider credentials and endpoint are always private to the process.
func (a *App) aiOperations(jobID string) ([]map[string]any, error) {
	rows, err := a.DB.Query(`SELECT operation_key,name,status,attempt,model,response_id,input_tokens,output_tokens,usage_reported,error FROM ai_operations WHERE job_id=? ORDER BY created`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, name, status, model, responseID, message string
		var attempt int
		var input, output int64
		var usage bool
		if err = rows.Scan(&key, &name, &status, &attempt, &model, &responseID, &input, &output, &usage, &message); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"operation_key": key, "name": name, "status": status, "attempt": attempt, "model": model, "response_id": responseID, "input_tokens": input, "output_tokens": output, "usage_reported": usage, "error": message})
	}
	return out, rows.Err()
}
