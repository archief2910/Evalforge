package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spans (
  id TEXT PRIMARY KEY,
  trace_id TEXT NOT NULL,
  parent_id TEXT,
  name TEXT NOT NULL,
  type TEXT NOT NULL DEFAULT 'task',
  model TEXT,
  input TEXT, output TEXT, error TEXT, metadata TEXT,
  start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, latency_ms INTEGER NOT NULL,
  prompt_tokens INTEGER DEFAULT 0, completion_tokens INTEGER DEFAULT 0,
  cost_usd REAL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'ok',
  project_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_spans_project ON spans(project_id);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id);
CREATE TABLE IF NOT EXISTS feedback (
  id TEXT PRIMARY KEY, trace_id TEXT, span_id TEXT, kind TEXT NOT NULL,
  value TEXT, comment TEXT, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS datasets (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
  description TEXT DEFAULT '', version INTEGER DEFAULT 1, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS dataset_records (
  id TEXT PRIMARY KEY, dataset_id TEXT NOT NULL,
  input TEXT NOT NULL, expected TEXT, metadata TEXT, tags TEXT,
  source TEXT DEFAULT 'manual', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS scorers (
  id TEXT PRIMARY KEY, project_id TEXT, name TEXT NOT NULL,
  kind TEXT NOT NULL, config TEXT, description TEXT DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS experiments (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
  dataset_id TEXT, baseline_experiment_id TEXT,
  status TEXT NOT NULL DEFAULT 'pending', summary TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS eval_runs (
  id TEXT PRIMARY KEY, experiment_id TEXT NOT NULL, record_id TEXT,
  input TEXT, expected TEXT, output TEXT, error TEXT,
  latency_ms INTEGER, scores TEXT, output_mode TEXT DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS prompts (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, version INTEGER NOT NULL,
  model TEXT, template TEXT, params TEXT, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS model_prices (
  model TEXT PRIMARY KEY, input_per_1k REAL, output_per_1k REAL
);
CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL DEFAULT 'eval',
  experiment_id TEXT NOT NULL, project_id TEXT NOT NULL, payload TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 2,
  lease_expires_at TEXT, last_error TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claimable ON jobs(status, created_at);
` + authSchema

var (
	db   *sql.DB
	dbMu sync.Mutex // serialises write transactions (SQLite allows one writer)

	// selfBaseURL is this server's own origin, handed to the worker as GO_SERVER.
	selfBaseURL = "http://localhost:8000"

	// dbFile is the resolved SQLite path, logged at boot so it is never a
	// mystery which database a running server is actually serving.
	dbFile string
)

// idSeq makes generated ids unique even when the wall clock has coarse
// granularity (Windows time.Now() resolution is ~15ms).
var idSeq atomic.Uint64

func newID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq.Add(1))
}

func j(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func parseJSONPtr(s string) interface{} {
	if s == "" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s // raw string fallback (matches Python pj behaviour loosely)
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------- models / price estimation ----------

var defaultPrices = map[string][2]float64{
	"gpt-4o":          {0.0025, 0.01},
	"gpt-4o-mini":     {0.00015, 0.0006},
	"gpt-4.1":         {0.002, 0.008},
	"claude-sonnet-4": {0.003, 0.015},
	"llama-3.1-8b":    {0.0001, 0.0002},
}

// priceFor uses the in-memory table only: the ingest path runs inside a
// transaction on a single-connection pool, so nested db.Query* calls deadlock.
func priceFor(model string) (float64, float64) {
	if p, ok := defaultPrices[model]; ok {
		return p[0], p[1]
	}
	return 0.001, 0.002
}

func estTokens(v interface{}) int {
	if v == nil {
		return 0
	}
	s, ok := v.(string)
	if !ok {
		b, _ := json.Marshal(v)
		s = string(b)
	}
	return len(s) / 4
}

// ---------- span ingest ----------

type spanEvent struct {
	ID               string      `json:"id,omitempty"`
	TraceID          string      `json:"trace_id,omitempty"`
	ParentID         string      `json:"parent_id,omitempty"`
	Name             string      `json:"name"`
	Type             string      `json:"type"`
	Model            string      `json:"model,omitempty"`
	Input            interface{} `json:"input,omitempty"`
	Output           interface{} `json:"output,omitempty"`
	Error            interface{} `json:"error,omitempty"`
	Metadata         interface{} `json:"metadata,omitempty"`
	StartMS          int64       `json:"start_ms,omitempty"`
	EndMS            int64       `json:"end_ms,omitempty"`
	PromptTokens     int         `json:"prompt_tokens,omitempty"`
	CompletionTokens int         `json:"completion_tokens,omitempty"`
}

type ingestIn struct {
	Events []spanEvent `json:"events"`
}

// traceIDForTx runs inside the caller's transaction (single-conn pool safety).
func traceIDForTx(tx *sql.Tx, parentID string) string {
	if parentID == "" {
		return ""
	}
	var tid string
	_ = tx.QueryRow("SELECT trace_id FROM spans WHERE id=?", parentID).Scan(&tid)
	return tid
}

func handleIngest(w http.ResponseWriter, r *http.Request, projectID string) {
	var in ingestIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, "bad json: "+err.Error())
		return
	}
	inserted, tracesCreated := 0, 0
	dbMu.Lock()
	defer dbMu.Unlock()
	tx, err := db.Begin()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	// --- pass 1: assign ids and resolve trace ids ---
	// A batch may arrive with children before their parent (the SDK buffers
	// spans and flushes on span end, so a child can precede its root), so the
	// trace id is resolved in a second pass instead of a single parent lookup.
	type prep struct {
		id, parent, trace string
		ev                spanEvent
	}
	preps := make([]prep, 0, len(in.Events))
	index := map[string]int{}
	for _, ev := range in.Events {
		id := ev.ID
		if id == "" {
			id = newID()
		}
		index[id] = len(preps)
		preps = append(preps, prep{id: id, parent: ev.ParentID, trace: ev.TraceID, ev: ev})
	}
	for i := 0; i < len(preps)+1; i++ {
		changed := false
		for pi := range preps {
			if preps[pi].trace != "" {
				continue
			}
			if preps[pi].parent == "" {
				preps[pi].trace = preps[pi].id // root starts its own trace
				changed = true
				continue
			}
			if pj, ok := index[preps[pi].parent]; ok && preps[pj].trace != "" {
				preps[pi].trace = preps[pj].trace
				changed = true
				continue
			}
			if t := traceIDForTx(tx, preps[pi].parent); t != "" {
				preps[pi].trace = t // parent came from an earlier batch
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	for pi := range preps {
		if preps[pi].trace == "" {
			preps[pi].trace = preps[pi].id // orphan span: its own trace
		}
	}

	// --- pass 2: insert ---
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, pr := range preps {
		ev := pr.ev
		id := pr.id
		traceID := pr.trace
		if ev.ParentID == "" && ev.TraceID == "" {
			tracesCreated++
		}
		start := ev.StartMS
		if start == 0 {
			start = time.Now().UnixMilli()
		}
		end := ev.EndMS
		if end == 0 {
			end = start + 100
		}
		latency := end - start
		if latency < 0 {
			latency = 0
		}
		status := "ok"
		if ev.Error != nil {
			status = "error"
		}
		model := ""
		if ev.Type == "llm" {
			model = ev.Model
		}
		pt, ct := ev.PromptTokens, ev.CompletionTokens
		var ip, op float64
		if model != "" {
			ip, op = priceFor(model)
			if pt == 0 {
				pt = estTokens(ev.Input)
			}
			if ct == 0 {
				ct = estTokens(ev.Output)
			}
		}
		cost := float64(pt)/1000*ip + float64(ct)/1000*op
		if err := insertSpanTx(tx, projectID, spanRow{
			ID: id, TraceID: traceID, ParentID: ev.ParentID, Name: ev.Name, Type: ev.Type,
			Model: model, Input: ev.Input, Output: ev.Output, Error: ev.Error, Metadata: ev.Metadata,
			StartMS: start, EndMS: end, LatencyMS: latency,
			PromptTokens: pt, CompletionTokens: ct, CostUSD: cost, Status: status,
		}, now); err != nil {
			errJSON(w, 500, "span insert: "+err.Error())
			return
		}
		inserted++
	}
	// roll up root latency from children + propagate child errors
	rollupTraceTx(tx)

	if err := tx.Commit(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"inserted": inserted, "traces_created": tracesCreated})
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func jsonOrNull(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	return j(v)
}

// ---------- trace queries ----------

type traceRow struct {
	Input, Output, Error, Metadata, Model interface{}
	TraceID, Name, Status                 string
	LatencyMS                             int64
	CostUSD                               float64
	SpanCount                             int
	StartMS                               int64
}

// A trace's model and cost live on its LLM child spans, not on the root task
// span, so both are aggregated over the whole trace.
const traceCostExpr = `(SELECT COALESCE(SUM(c.cost_usd),0) FROM spans c WHERE c.trace_id=s.trace_id)`
const traceModelsExpr = `(SELECT GROUP_CONCAT(DISTINCT ms.model) FROM spans ms WHERE ms.trace_id=s.trace_id AND ms.model IS NOT NULL AND ms.model<>'')`

func handleTraces(w http.ResponseWriter, r *http.Request, projectID string) {
	q := r.URL.Query()
	where := []string{"s.project_id=?", "s.parent_id IS NULL"}
	args := []interface{}{projectID}
	if v := q.Get("status"); v != "" {
		where = append(where, "s.status=?")
		args = append(args, v)
	}
	if v := q.Get("model"); v != "" {
		where = append(where, "EXISTS (SELECT 1 FROM spans ms WHERE ms.trace_id=s.trace_id AND ms.model=?)")
		args = append(args, v)
	}
	if v := q.Get("search"); v != "" {
		like := "%" + v + "%"
		where = append(where, "(s.input LIKE ? OR s.output LIKE ? OR s.name LIKE ? OR s.error LIKE ?)")
		args = append(args, like, like, like, like)
	}
	if v := q.Get("min_latency_ms"); v != "" {
		where = append(where, "s.latency_ms>=?")
		args = append(args, toF(v))
	}
	if v := q.Get("min_cost_usd"); v != "" {
		where = append(where, traceCostExpr+">=?")
		args = append(args, toF(v))
	}
	if q.Get("has_feedback") == "true" {
		where = append(where, "EXISTS (SELECT 1 FROM feedback f WHERE f.trace_id=s.trace_id)")
	}
	orderBy := "s.start_ms DESC"
	sortBy := q.Get("sort_by")
	if sortBy == "latency_ms" || sortBy == "cost_usd" || sortBy == "start_ms" {
		dir := "DESC"
		if strings.EqualFold(q.Get("sort_dir"), "asc") {
			dir = "ASC"
		}
		col := "s." + sortBy
		if sortBy == "cost_usd" {
			col = traceCostExpr
		}
		orderBy = col + " " + dir
	}
	limit, offset := 50, 0
	fmt.Sscanf(q.Get("limit"), "%d", &limit)
	fmt.Sscanf(q.Get("offset"), "%d", &offset)

	whereSQL := strings.Join(where, " AND ")
	rows, err := db.Query(`SELECT s.trace_id, s.name, s.input, s.output, s.error, s.metadata,
		`+traceModelsExpr+` AS models, s.status, s.latency_ms,
		`+traceCostExpr+` AS trace_cost, s.start_ms,
		(SELECT COUNT(*) FROM spans c WHERE c.trace_id=s.trace_id) AS span_count
		FROM spans s WHERE `+whereSQL+` ORDER BY `+orderBy+` LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		m := map[string]interface{}{}
		var traceID, name, status string
		var input, output, errStr, metadata, models sql.NullString
		var latency, startMS int64
		var cost float64
		var spanCount int
		if err := rows.Scan(&traceID, &name, &input, &output, &errStr, &metadata, &models, &status, &latency, &cost, &startMS, &spanCount); err != nil {
			continue
		}
		m["trace_id"] = traceID
		m["name"] = name
		m["input"] = parseJSONPtr(input.String)
		m["output"] = parseJSONPtr(output.String)
		m["error"] = parseJSONPtr(errStr.String)
		m["metadata"] = parseJSONPtr(metadata.String)
		m["model"] = models.String
		m["status"] = status
		m["latency_ms"] = latency
		m["cost_usd"] = cost
		m["start_ms"] = startMS
		m["span_count"] = spanCount
		out = append(out, m)
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	total := 0
	db.QueryRow(`SELECT COUNT(*) FROM spans s WHERE `+whereSQL, args...).Scan(&total)
	writeJSON(w, 200, map[string]interface{}{"traces": out, "total": total})
}

func toF(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func spanTree(root map[string]interface{}, all map[string]map[string]interface{}) []map[string]interface{} {
	kids := []map[string]interface{}{}
	for _, m := range all {
		if pid, ok := m["parent_id"].(string); ok && pid == root["id"].(string) {
			m["children"] = spanTree(m, all)
			kids = append(kids, m)
		}
	}
	sort.Slice(kids, func(i, j int) bool {
		a, _ := kids[i]["start_ms"].(int64)
		b, _ := kids[j]["start_ms"].(int64)
		return a < b
	})
	return kids
}

func handleTraceDetail(w http.ResponseWriter, r *http.Request, projectID, traceID string) {
	rows, err := db.Query(`SELECT id, trace_id, parent_id, name, type, model, input, output, error,
		metadata, start_ms, end_ms, latency_ms, prompt_tokens, completion_tokens, cost_usd, status
		FROM spans WHERE trace_id=? AND project_id=? ORDER BY start_ms`, traceID, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	all := map[string]map[string]interface{}{}
	order := []string{}
	for rows.Next() {
		var id, tid, name, typ, status string
		var parentID, model sql.NullString
		var input, output, errStr, metadata sql.NullString
		var start, end, latency int64
		var pt, ct int
		var cost float64
		if err := rows.Scan(&id, &tid, &parentID, &name, &typ, &model, &input, &output, &errStr, &metadata, &start, &end, &latency, &pt, &ct, &cost, &status); err != nil {
			continue
		}
		m := map[string]interface{}{
			"id": id, "trace_id": tid, "name": name, "type": typ, "model": model.String,
			"input": parseJSONPtr(input.String), "output": parseJSONPtr(output.String),
			"error": parseJSONPtr(errStr.String), "metadata": parseJSONPtr(metadata.String),
			"start_ms": start, "end_ms": end, "latency_ms": latency,
			"prompt_tokens": pt, "completion_tokens": ct, "cost_usd": cost, "status": status,
		}
		if parentID.Valid {
			m["parent_id"] = parentID.String
		}
		all[id] = m
		order = append(order, id)
	}
	roots := []map[string]interface{}{}
	for _, id := range order {
		m := all[id]
		pid := ""
		if v, ok := m["parent_id"].(string); ok {
			pid = v
		}
		if pid == "" {
			m["children"] = spanTree(m, all)
			roots = append(roots, m)
		}
	}
	fb := []map[string]interface{}{}
	frows, err := db.Query(`SELECT id, trace_id, span_id, kind, value, comment, created_at FROM feedback WHERE trace_id=? ORDER BY created_at DESC`, traceID)
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var fid, tid, kind, createdAt string
			var spanID, value, comment sql.NullString
			frows.Scan(&fid, &tid, &spanID, &kind, &value, &comment, &createdAt)
			fb = append(fb, map[string]interface{}{
				"id": fid, "trace_id": tid, "kind": kind, "value": parseJSONPtr(value.String),
				"comment": comment.String, "created_at": createdAt,
			})
		}
	}
	if fb == nil {
		fb = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{"trace_id": traceID, "span_tree": roots, "feedback": fb})
}

type feedbackIn struct {
	Kind    string      `json:"kind"`
	Value   interface{} `json:"value"`
	Comment string      `json:"comment"`
	SpanID  string      `json:"span_id"`
}

func handleFeedback(w http.ResponseWriter, r *http.Request, projectID, traceID string) {
	var in feedbackIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	id := newID()
	val := ""
	if s, ok := in.Value.(string); ok {
		val = s
	} else if in.Value != nil {
		val = j(in.Value)
	}
	dbMu.Lock()
	_, err := db.Exec(`INSERT INTO feedback(id, trace_id, span_id, kind, value, comment, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, traceID, nilIfEmpty(in.SpanID), in.Kind, val, in.Comment, time.Now().UTC().Format(time.RFC3339Nano))
	dbMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

// ---------- dashboard ----------

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	k := (float64(len(sorted)) - 1) * p
	lo := int(k)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	return sorted[lo]*(float64(hi)-k) + sorted[hi]*(k-float64(lo))
}

func handleDashboard(w http.ResponseWriter, r *http.Request, projectID string) {
	hours := 168
	fmt.Sscanf(r.URL.Query().Get("hours"), "%d", &hours)
	since := time.Now().UnixMilli() - int64(hours)*3600*1000

	rootRows, err := db.Query(`SELECT latency_ms, status, name, start_ms FROM spans
		WHERE project_id=? AND parent_id IS NULL AND start_ms>=?`, projectID, since)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type rootRowT struct {
		latency      int64
		status, name string
		startMS      int64
	}
	var rootRowsL []rootRowT
	for rootRows.Next() {
		var rr rootRowT
		rootRows.Scan(&rr.latency, &rr.status, &rr.name, &rr.startMS)
		rootRowsL = append(rootRowsL, rr)
	}
	rootRows.Close()

	allRows, err := db.Query(`SELECT model, prompt_tokens, completion_tokens, cost_usd, start_ms FROM spans
		WHERE project_id=? AND start_ms>=?`, projectID, since)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	type allRowT struct {
		model   string
		pt, ct  int
		cost    float64
		startMS int64
	}
	var all []allRowT
	for allRows.Next() {
		var ar allRowT
		allRows.Scan(&ar.model, &ar.pt, &ar.ct, &ar.cost, &ar.startMS)
		all = append(all, ar)
	}
	allRows.Close()

	lat := []float64{}
	bucketMS := int64(3600 * 1000)
	if hours > 72 {
		bucketMS = 24 * 3600 * 1000
	}
	type bucket struct {
		ts        int64
		requests  int
		errors    int
		p50, p95  int64
		latencies []float64
	}
	buckets := map[int64]*bucket{}
	modelCounts := map[string]int{}
	var totalTokens int
	var totalCost float64
	for _, rr := range rootRowsL {
		lat = append(lat, float64(rr.latency))
		b := int64(rr.startMS / bucketMS * bucketMS)
		if buckets[b] == nil {
			buckets[b] = &bucket{ts: b}
		}
		buckets[b].requests++
		if rr.status == "error" {
			buckets[b].errors++
		}
		buckets[b].latencies = append(buckets[b].latencies, float64(rr.latency))
	}
	for _, ar := range all {
		totalTokens += ar.pt + ar.ct
		totalCost += ar.cost
		if ar.model != "" {
			modelCounts[ar.model]++
		}
	}
	sort.Float64s(lat)
	n := len(rootRowsL)
	errors := 0
	for _, rr := range rootRowsL {
		if rr.status == "error" {
			errors++
		}
	}
	ts := []map[string]interface{}{}
	keys := []int64{}
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		b := buckets[k]
		sort.Float64s(b.latencies)
		p50 := int64(percentile(b.latencies, 0.5))
		p95 := int64(percentile(b.latencies, 0.95))
		ts = append(ts, map[string]interface{}{
			"ts": k, "requests": b.requests, "errors": b.errors,
			"latency_p50": p50, "latency_p95": p95,
		})
	}
	if ts == nil {
		ts = []map[string]interface{}{}
	}
	mb := []map[string]interface{}{}
	// stable order by count desc
	type mc struct {
		m string
		c int
	}
	var mcs []mc
	for m, c := range modelCounts {
		mcs = append(mcs, mc{m, c})
	}
	sort.Slice(mcs, func(i, j int) bool { return mcs[i].c > mcs[j].c })
	for _, x := range mcs {
		mb = append(mb, map[string]interface{}{"model": x.m, "count": x.c})
	}
	if mb == nil {
		mb = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"requests": n,
		"errors":   errors,
		"error_rate": func() float64 {
			if n == 0 {
				return 0
			}
			return float64(errors) / float64(n)
		}(),
		"latency": map[string]interface{}{
			"p50": int64(percentile(lat, 0.5)), "p95": int64(percentile(lat, 0.95)),
			"p99": int64(percentile(lat, 0.99)),
			"mean": func() int64 {
				if n == 0 {
					return 0
				}
				var s float64
				for _, v := range lat {
					s += v
				}
				return int64(s / float64(n))
			}(),
		},
		"tokens": totalTokens, "cost_usd": totalCost,
		"span_count":      len(all),
		"model_breakdown": mb,
		"timeseries":      ts,
	})
}

// ---------- projects ----------

type projectIn struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func handleProjects(w http.ResponseWriter, r *http.Request, _ string) {
	switch r.Method {
	case http.MethodPost:
		var in projectIn
		json.NewDecoder(r.Body).Decode(&in)
		id := newID()
		dbMu.Lock()
		db.Exec(`INSERT INTO projects(id, name, description, created_at) VALUES (?,?,?,?)`,
			id, in.Name, in.Description, time.Now().UTC().Format(time.RFC3339Nano))
		dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := db.Query(`SELECT id, name, description, created_at FROM projects ORDER BY created_at DESC`)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		// A project-scoped credential sees exactly one project in the list —
		// filtered rather than swapped for a single object, so the response
		// shape never depends on the credential.
		scope := ""
		if p := principalFrom(r.Context()); p != nil {
			scope = p.ProjectID
		}
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, desc, created string
			rows.Scan(&id, &name, &desc, &created)
			if scope != "" && id != scope {
				continue
			}
			out = append(out, map[string]interface{}{"id": id, "name": name, "description": desc, "created_at": created})
		}
		writeJSON(w, 200, out)
	}
}

// handleProjectOne serves a single project (GET /api/v1/projects/{id}).
func handleProjectOne(w http.ResponseWriter, r *http.Request, _ string, pid string) {
	var id, name, desc, created string
	err := db.QueryRow(`SELECT id, name, description, created_at FROM projects WHERE id=?`, pid).
		Scan(&id, &name, &desc, &created)
	if err != nil {
		errJSON(w, 404, "project not found")
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "name": name, "description": desc, "created_at": created,
	})
}

// ---------- datasets ----------

type recordIn struct {
	Input    interface{} `json:"input"`
	Expected interface{} `json:"expected"`
	Metadata interface{} `json:"metadata"`
	Tags     []string    `json:"tags"`
}

func handleDatasets(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodPost:
		var in projectIn
		json.NewDecoder(r.Body).Decode(&in)
		id := newID()
		dbMu.Lock()
		db.Exec(`INSERT INTO datasets(id, project_id, name, description, version, created_at) VALUES (?,?,?,?,1,?)`,
			id, projectID, in.Name, in.Description, time.Now().UTC().Format(time.RFC3339Nano))
		dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := db.Query(`SELECT d.id, d.name, d.description, d.version, d.created_at,
			(SELECT COUNT(*) FROM dataset_records r WHERE r.dataset_id=d.id) AS record_count
			FROM datasets d WHERE d.project_id=? ORDER BY d.created_at DESC`, projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, desc, created string
			var version, count int
			rows.Scan(&id, &name, &desc, &version, &created, &count)
			out = append(out, map[string]interface{}{"id": id, "name": name, "description": desc, "version": version, "created_at": created, "record_count": count})
		}
		writeJSON(w, 200, out)
	}
}

func handleDatasetOne(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	var id, projectID, name, desc, created string
	var version int
	err := db.QueryRow(`SELECT id, project_id, name, description, version, created_at FROM datasets WHERE id=?`, dsID).
		Scan(&id, &projectID, &name, &desc, &version, &created)
	if err != nil {
		errJSON(w, 404, "dataset not found")
		return
	}
	var projectName string
	db.QueryRow(`SELECT name FROM projects WHERE id=?`, projectID).Scan(&projectName)
	records := []map[string]interface{}{}
	rows, err := db.Query(`SELECT id, input, expected, metadata, tags, source, created_at FROM dataset_records WHERE dataset_id=? ORDER BY created_at DESC`, dsID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var rid, rinput, rsource, rcreated string
			var rexpected, rmeta, rtags sql.NullString
			rows.Scan(&rid, &rinput, &rexpected, &rmeta, &rtags, &rsource, &rcreated)
			rec := map[string]interface{}{
				"id": rid, "input": parseJSONPtr(rinput), "expected": parseJSONPtr(rexpected.String),
				"metadata": parseJSONPtr(rmeta.String), "source": rsource, "created_at": rcreated,
			}
			if rtags.String != "" {
				var tags []string
				json.Unmarshal([]byte(rtags.String), &tags)
				rec["tags"] = tags
			} else {
				rec["tags"] = []string{}
			}
			records = append(records, rec)
		}
	}
	if records == nil {
		records = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "project_id": projectID, "name": name, "description": desc,
		"version": version, "created_at": created, "project_name": projectName,
		"records": records,
	})
}

// handleAddRecord accepts either a single record object or an array of records.
// A batch is inserted in one transaction and bumps the dataset version once.
func handleAddRecord(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		errJSON(w, 400, "read body: "+err.Error())
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		errJSON(w, 400, "empty body")
		return
	}
	var records []recordIn
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &records); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
	} else {
		var one recordIn
		if err := json.Unmarshal(trimmed, &one); err != nil {
			errJSON(w, 400, "bad json: "+err.Error())
			return
		}
		records = []recordIn{one}
	}
	if len(records) == 0 {
		writeJSON(w, 200, map[string]interface{}{"ids": []string{}})
		return
	}
	ids := make([]string, 0, len(records))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	dbMu.Lock()
	tx, err := db.Begin()
	if err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	for _, in := range records {
		id := newID()
		rectags := in.Tags
		if rectags == nil {
			rectags = []string{}
		}
		if _, err := tx.Exec(`INSERT INTO dataset_records(id, dataset_id, input, expected, metadata, tags, source, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			id, dsID, j(in.Input), jsonOrNull(in.Expected), jsonOrNull(in.Metadata), j(rectags), "manual", now); err != nil {
			dbMu.Unlock()
			errJSON(w, 500, err.Error())
			return
		}
		ids = append(ids, id)
	}
	tx.Exec(`UPDATE datasets SET version=version+1 WHERE id=?`, dsID)
	if err := tx.Commit(); err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	dbMu.Unlock()
	if len(ids) == 1 {
		writeJSON(w, 200, map[string]string{"id": ids[0]})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ids": ids})
}

func handleRecordFromTrace(w http.ResponseWriter, r *http.Request, _ string, dsID string) {
	var in struct {
		TraceID  string      `json:"trace_id"`
		Expected interface{} `json:"expected"`
		Tags     []string    `json:"tags"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	// find project + root input from trace
	var projectID, rootInput string
	err := db.QueryRow(`SELECT s.project_id, s.input FROM spans s
		JOIN datasets d ON d.project_id = s.project_id
		WHERE s.trace_id=? AND s.parent_id IS NULL LIMIT 1`, in.TraceID).Scan(&projectID, &rootInput)
	if err != nil {
		errJSON(w, 404, "trace not found")
		return
	}
	id := newID()
	tags := in.Tags
	if tags == nil {
		tags = []string{"from-trace"}
	}
	meta := map[string]string{"origin_trace_id": in.TraceID, "source": "review"}
	dbMu.Lock()
	_, err = db.Exec(`INSERT INTO dataset_records(id, dataset_id, input, expected, metadata, tags, source, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		id, dsID, rootInput, jsonOrNull(in.Expected), j(meta), j(tags), "review",
		time.Now().UTC().Format(time.RFC3339Nano))
	db.Exec(`UPDATE datasets SET version=version+1 WHERE id=?`, dsID)
	dbMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id})
}

// ---------- prompts ----------

func handlePrompts(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodPost:
		var in struct {
			Name     string      `json:"name"`
			Model    string      `json:"model"`
			Template interface{} `json:"template"`
			Params   interface{} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		var ver int
		db.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM prompts WHERE project_id=? AND name=?`, projectID, in.Name).Scan(&ver)
		id := newID()
		dbMu.Lock()
		db.Exec(`INSERT INTO prompts(id, project_id, name, version, model, template, params, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, projectID, in.Name, ver, in.Model, j(in.Template), j(in.Params), time.Now().UTC().Format(time.RFC3339Nano))
		dbMu.Unlock()
		writeJSON(w, 200, map[string]string{"id": id})
	default:
		rows, err := db.Query(`SELECT id, name, version, model, template, params, created_at FROM prompts WHERE project_id=? ORDER BY name, version DESC`, projectID)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]interface{}{}
		for rows.Next() {
			var id, name, created string
			var version int
			var model, template, params sql.NullString
			rows.Scan(&id, &name, &version, &model, &template, &params, &created)
			out = append(out, map[string]interface{}{
				"id": id, "name": name, "version": version, "model": model.String,
				"template": parseJSONPtr(template.String), "params": parseJSONPtr(params.String), "created_at": created,
			})
		}
		writeJSON(w, 200, out)
	}
}

// ---------- scorers (registry served by Go; execution in worker) ----------

var builtinScorers = []map[string]interface{}{
	{"name": "exact_match", "kind": "deterministic", "description": "Case-insensitive exact match against expected"},
	{"name": "contains", "kind": "deterministic", "description": "Expected substring present in output"},
	{"name": "levenshtein_similarity", "kind": "deterministic", "description": "1 - edit distance / max length"},
	{"name": "json_valid", "kind": "deterministic", "description": "Output parses as valid JSON"},
	{"name": "numeric_diff", "kind": "deterministic", "description": "Numeric closeness of first number in output vs expected"},
	{"name": "regex_guard", "kind": "deterministic", "description": "Pattern guard (must_match / must_not_match), e.g. no PII"},
	{"name": "factuality_judge", "kind": "llm_judge", "description": "LLM-as-judge: factual correctness vs expected"},
	{"name": "helpfulness_judge", "kind": "llm_judge", "description": "LLM-as-judge: helpfulness of the response"},
}

func handleScorers(w http.ResponseWriter, r *http.Request, _ ...string) {
	writeJSON(w, 200, builtinScorers)
}

// ---------- evaluations (Go dispatches, Python worker runs) ----------

type evalIn struct {
	DatasetID            string   `json:"dataset_id"`
	ExperimentName       string   `json:"experiment_name"`
	ScorerNames          []string `json:"scorer_names"`
	BaselineExperimentID string   `json:"baseline_experiment_id"`
}

func handleStartEval(w http.ResponseWriter, r *http.Request, projectID string) {
	var in evalIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	expID := newID()
	// The experiment and its job is one transaction: a crash between them would
	// create an experiment nobody owns.
	dbMu.Lock()
	tx, err := db.Begin()
	if err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO experiments(id, project_id, name, dataset_id, baseline_experiment_id, status, created_at)
		VALUES (?,?,?,?,?,'running',?)`,
		expID, projectID, in.ExperimentName, in.DatasetID, nilIfEmpty(in.BaselineExperimentID), nowUTC())
	if err == nil {
		err = enqueueEvalJobTx(tx, expID, projectID, in)
	}
	if err == nil {
		err = tx.Commit()
	}
	dbMu.Unlock()
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	// The dispatcher claims it within one tick; the request only enqueues.
	writeJSON(w, 200, map[string]string{"experiment_id": expID})
}

// workerEnv builds the worker's environment with GO_SERVER pointing at this
// server, so a non-default ADDR (or a second instance) still reports back here.
func workerEnv(selfBase string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GO_SERVER=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GO_SERVER="+selfBase)
}

// runWorkerJob executes one worker subprocess and reports the outcome as
// (diagnostic text, timedOut). It never mutates the experiment: the job layer
// owns status transitions so there is exactly one writer for them.
func runWorkerJob(parent context.Context, expID string, in evalIn, projectID string) (string, bool) {
	workerPath := os.Getenv("WORKER_PATH")
	if workerPath == "" {
		workerPath = filepath.Join("..", "worker-py", "worker.py")
	}
	// A free/remote provider can stall: bound the job so an experiment never
	// sits in "running" forever.
	timeout := 12 * time.Minute
	if v := os.Getenv("WORKER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python", workerPath, "--experiment-id", expID, "--project-id", projectID,
		"--dataset-id", in.DatasetID, "--experiment-name", in.ExperimentName,
		"--baseline-id", in.BaselineExperimentID, "--scorers", strings.Join(in.ScorerNames, ","))
	cmd.Env = workerEnv(selfBaseURL)
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	switch {
	case parent.Err() != nil:
		return "released on shutdown", false
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("worker timed out after %s", timeout), true
	case err != nil:
		log.Printf("worker failed for %s: %v\n%s", expID, err, tail)
		if strings.TrimSpace(tail) == "" {
			tail = err.Error()
		}
		return tail, false
	}
	log.Printf("worker done for %s\n%s", expID, tail)
	return "", false
}

func handleExperimentDetail(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	var id, projectID, name, datasetID, status, created string
	var baseline sql.NullString
	var summary sql.NullString
	err := db.QueryRow(`SELECT id, project_id, name, dataset_id, baseline_experiment_id, status, summary, created_at
		FROM experiments WHERE id=?`, expID).Scan(&id, &projectID, &name, &datasetID, &baseline, &status, &summary, &created)
	if err != nil {
		errJSON(w, 404, "experiment not found")
		return
	}
	var datasetName string
	db.QueryRow(`SELECT name FROM datasets WHERE id=?`, datasetID).Scan(&datasetName)
	runs := []map[string]interface{}{}
	rows, err := db.Query(`SELECT id, record_id, input, expected, output, error, latency_ms, scores, output_mode, created_at
		FROM eval_runs WHERE experiment_id=? ORDER BY created_at`, expID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var rid, recID, rinput, rcreated string
			var rexpected, routput, rerror, rscores, rmode sql.NullString
			var lat int
			rows.Scan(&rid, &recID, &rinput, &rexpected, &routput, &rerror, &lat, &rscores, &rmode, &rcreated)
			runs = append(runs, map[string]interface{}{
				"id": rid, "record_id": recID, "input": parseJSONPtr(rinput), "expected": parseJSONPtr(rexpected.String),
				"output": parseJSONPtr(routput.String), "error": parseJSONPtr(rerror.String),
				"latency_ms": lat, "scores": parseJSONPtr(rscores.String), "output_mode": rmode.String,
				"created_at": rcreated,
			})
		}
	}
	if runs == nil {
		runs = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"id": id, "project_id": projectID, "name": name, "dataset_id": datasetID,
		"dataset_name":           datasetName,
		"baseline_experiment_id": baseline.String, "status": status,
		"summary": parseJSONPtr(summary.String), "created_at": created, "runs": runs,
	})
}

func handleExperimentsList(w http.ResponseWriter, r *http.Request, projectID string) {
	rows, err := db.Query(`SELECT e.id, e.name, e.dataset_id, e.baseline_experiment_id, e.status, e.summary, e.created_at,
		d.name AS dataset_name,
		(SELECT COUNT(*) FROM eval_runs r WHERE r.experiment_id=e.id) AS run_count
		FROM experiments e LEFT JOIN datasets d ON d.id=e.dataset_id
		WHERE e.project_id=? ORDER BY e.created_at DESC`, projectID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, name, datasetID, status, created, datasetName string
		var baseline, summary sql.NullString
		var runCount int
		rows.Scan(&id, &name, &datasetID, &baseline, &status, &summary, &created, &datasetName, &runCount)
		out = append(out, map[string]interface{}{
			"id": id, "name": name, "dataset_id": datasetID, "dataset_name": datasetName,
			"baseline_experiment_id": baseline.String, "status": status,
			"summary": parseJSONPtr(summary.String), "created_at": created, "run_count": runCount,
		})
	}
	writeJSON(w, 200, out)
}

func handleCompare(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	baselineID := r.URL.Query().Get("baseline_id")
	base := map[string]map[string]interface{}{}
	rows, err := db.Query(`SELECT input, output, expected, scores FROM eval_runs WHERE experiment_id=?`, baselineID)
	if err == nil {
		for rows.Next() {
			var input, output, expected, scores string
			rows.Scan(&input, &output, &expected, &scores)
			key := input
			base[key] = map[string]interface{}{
				"output": parseJSONPtr(output), "expected": parseJSONPtr(expected), "scores": parseJSONPtr(scores),
			}
		}
		rows.Close()
	}
	rows, err = db.Query(`SELECT input, output, expected, scores, error, latency_ms, output_mode FROM eval_runs WHERE experiment_id=?`, expID)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var input, output, expected, scores string
		var errStr, omode sql.NullString
		var lat int
		rows.Scan(&input, &output, &expected, &scores, &errStr, &lat, &omode)
		sc := parseJSONPtr(scores)
		delta := map[string]interface{}{}
		baseScores := map[string]interface{}{}
		if b, ok := base[input]; ok {
			if bs, ok := b["scores"].(map[string]interface{}); ok {
				baseScores = bs
			}
		}
		if sm, ok := sc.(map[string]interface{}); ok {
			for name, sv := range sm {
				if svm, ok := sv.(map[string]interface{}); ok {
					ns, _ := svm["score"].(float64)
					bv := 0.0
					if bsm, ok := baseScores[name].(map[string]interface{}); ok {
						if bf, ok := bsm["score"].(float64); ok {
							bv = bf
						}
					}
					delta[name] = ns - bv
				}
			}
		}
		row := map[string]interface{}{
			"input": parseJSONPtr(input), "output": parseJSONPtr(output), "expected": parseJSONPtr(expected),
			"scores": sc, "baseline_output": nil, "baseline_scores": nil, "delta": delta,
			"error": parseJSONPtr(errStr.String), "latency_ms": lat, "output_mode": omode.String,
		}
		if b, ok := base[input]; ok {
			row["baseline_output"] = b["output"]
			row["baseline_scores"] = b["scores"]
		}
		out = append(out, row)
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	writeJSON(w, 200, out)
}

// ---------- migration from legacy sqlite (optional) ----------

func migrateLegacy() {
	legacy := os.Getenv("LEGACY_DB")
	if legacy == "" {
		return
	}
	fmt.Println("migrating from", legacy)
	ldb, err := sql.Open("sqlite", legacy)
	if err != nil {
		log.Println("legacy open failed:", err)
		return
	}
	defer ldb.Close()
	type pair struct {
		table string
		cols  string
	}
	for _, p := range []pair{
		{"projects", "id,name,description,created_at"},
		{"spans", "id,trace_id,parent_id,name,type,model,input,output,error,metadata,start_ms,end_ms,latency_ms,prompt_tokens,completion_tokens,cost_usd,status,project_id,created_at"},
		{"feedback", "id,trace_id,span_id,kind,value,comment,created_at"},
		{"datasets", "id,project_id,name,description,version,created_at"},
		{"dataset_records", "id,dataset_id,input,expected,metadata,tags,source,created_at"},
		{"experiments", "id,project_id,name,dataset_id,baseline_experiment_id,status,summary,created_at"},
		{"eval_runs", "id,experiment_id,record_id,input,expected,output,error,latency_ms,scores,created_at"},
		{"prompts", "id,project_id,name,version,model,template,params,created_at"},
	} {
		rows, err := ldb.Query("SELECT " + p.cols + " FROM " + p.table)
		if err != nil {
			log.Printf("migrate %s: %v", p.table, err)
			continue
		}
		cols := strings.Split(p.cols, ",")
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
		var n int
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			db.Exec("INSERT OR IGNORE INTO "+p.table+"("+p.cols+") VALUES ("+placeholders+")", vals...)
			n++
		}
		rows.Close()
		fmt.Printf("migrated %d rows into %s\n", n, p.table)
	}
}

// ---------- worker: execute eval runs in Python ----------
// The worker generates task outputs and computes scores, then writes eval_runs + summary
// over an HTTP callback (see worker.py).

func handleWorkerCallback(w http.ResponseWriter, r *http.Request, _ string, expID string) {
	var in struct {
		Runs []struct {
			RecordID   string                 `json:"record_id"`
			Input      interface{}            `json:"input"`
			Expected   interface{}            `json:"expected"`
			Output     interface{}            `json:"output"`
			Error      interface{}            `json:"error"`
			LatencyMS  int                    `json:"latency_ms"`
			Scores     map[string]interface{} `json:"scores"`
			OutputMode string                 `json:"output_mode"`
		} `json:"runs"`
		Summary map[string]interface{} `json:"summary"`
		Status  string                 `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	dbMu.Lock()
	tx, err := db.Begin()
	if err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	// The callback is idempotent: a re-dispatched or retried worker must replace
	// its experiment's runs instead of appending a second copy.
	if _, err := tx.Exec(`DELETE FROM eval_runs WHERE experiment_id=?`, expID); err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	for _, run := range in.Runs {
		tx.Exec(`INSERT INTO eval_runs(id, experiment_id, record_id, input, expected, output, error, latency_ms, scores, output_mode, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			newID(), expID, nilIfEmpty(run.RecordID), j(run.Input), jsonOrNull(run.Expected),
			jsonOrNull(run.Output), jsonOrNull(run.Error), run.LatencyMS, j(run.Scores),
			run.OutputMode, time.Now().UTC().Format(time.RFC3339Nano))
	}
	tx.Exec(`UPDATE experiments SET summary=?, status=? WHERE id=?`, j(in.Summary), in.Status, expID)
	if err := tx.Commit(); err != nil {
		dbMu.Unlock()
		errJSON(w, 500, err.Error())
		return
	}
	dbMu.Unlock()
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// ---------- sqlite helpers ----------

func openDB() error {
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = filepath.Join("..", "data")
	}
	_ = os.MkdirAll(dir, 0o755)
	// WAL lets readers run while a writer holds the transaction, which is why
	// the single-connection pool (and the read-side lock) is not needed.
	// busy_timeout covers the remaining writer/writer contention.
	dbFile = filepath.Join(dir, "ai_eval.db")
	dsn := "file:" + dbFile +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	var err error
	db, err = sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	for m, p := range defaultPrices {
		db.Exec(`INSERT OR IGNORE INTO model_prices(model, input_per_1k, output_per_1k) VALUES (?,?,?)`, m, p[0], p[1])
	}
	db.Exec(`INSERT OR IGNORE INTO model_prices(model, input_per_1k, output_per_1k) VALUES ('*default*', 0.001, 0.002)`)
	// additive migrations for databases created by an earlier build
	ensureColumn(db, "eval_runs", "output_mode", "TEXT DEFAULT ''")
	return nil
}

// ensureColumn adds a column to an existing table when it is missing
// (CREATE TABLE IF NOT EXISTS does not evolve an already-created table).
func ensureColumn(db *sql.DB, table, col, typ string) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return
	}
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk) == nil && name == col {
			found = true
		}
	}
	rows.Close()
	if !found {
		if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + col + " " + typ); err != nil {
			log.Printf("migrate %s.%s: %v", table, col, err)
		} else {
			fmt.Printf("migrated column %s.%s\n", table, col)
		}
	}
}

// ---- health ----

func handleHealth(w http.ResponseWriter, r *http.Request, _ ...string) {
	ok := db.Ping() == nil
	writeJSON(w, 200, map[string]interface{}{"ok": ok, "jobs": jobCounts()})
}

// ---- routing ----

type route struct {
	pattern string
	handler func(w http.ResponseWriter, r *http.Request, tail ...string)
}

// loadDotEnv reads KEY=VALUE lines from a .env file into the process environment
// (without overriding existing vars and without logging values).
func loadDotEnv() {
	candidates := []string{".env", filepath.Join("..", ".env")}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				continue
			}
			if _, exists := os.LookupEnv(kv[0]); !exists {
				os.Setenv(kv[0], kv[1])
			}
		}
		return
	}
}

func main() {
	loadDotEnv()
	// --mcp runs the Model Context Protocol server (stdio) instead of the HTTP
	// API. It is an API client, so it needs no database handle of its own.
	if hasFlag("--mcp") {
		runMCP()
		return
	}
	// --create-key bootstraps the first credential of an auth-required
	// deployment without opening an HTTP port. The key goes to stdout.
	if hasFlag("--create-key") {
		args, _ := flagValue("--create-key")
		mintAPIKey(strings.Fields(args))
		return
	}
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(lvl)); err == nil {
			logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
		}
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8000"
	}
	if strings.HasPrefix(addr, ":") {
		selfBaseURL = "http://127.0.0.1" + addr
	} else {
		selfBaseURL = "http://" + addr
	}
	// Tool calls made over POST /mcp loop back to this same process, so the MCP
	// client has to address the port the server actually listens on rather than
	// the compiled-in default. An explicit MCP_API_BASE still wins (that is the
	// stdio case, where the API is a different process).
	if v := os.Getenv("MCP_API_BASE"); v != "" {
		mcpBaseURL = strings.TrimRight(v, "/")
	} else {
		mcpBaseURL = selfBaseURL
	}
	if err := openDB(); err != nil {
		log.Fatal("db open: ", err)
	}
	migrateLegacy()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Crash recovery: this process is the only writer, so a job still marked
	// running at boot belongs to a dead process. Requeue immediately rather
	// than waiting out its lease.
	if n := requeueOrphanedJobs(); n > 0 {
		logf("requeued orphaned jobs", "count", n)
	}
	dispatcher := startDispatcher(ctx)
	startAuthScrubber(ctx)

	mux := newRouter()

	srv := &http.Server{
		Addr:              addr,
		Handler:           withMiddleware(authenticate(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      5 * time.Minute, // long dashboard/compare queries
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logf("server listening", "addr", addr, "db", dbFile)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatal(err)
	case <-ctx.Done():
		logf("shutdown signal received; draining")
	}

	// Drain in-flight requests, then close the pool. Anything the worker is
	// still posting will be retried by the worker or fail its experiment
	// cleanly, instead of being killed mid-write.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	// The dispatcher's context is already cancelled by the signal; give the
	// running job a moment to release its lease before the pool closes.
	done := make(chan struct{})
	go func() { dispatcher.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Printf("shutdown: dispatcher still stopping after 15s")
	}
	if err := db.Close(); err != nil {
		log.Printf("db close: %v", err)
	}
	logf("shutdown complete")
}

// newRouter builds the complete route table. main() and the tests share it,
// so a test exercises exactly the wiring the server ships.
func newRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		handleHealth(w, r)
	})
	// OTLP/HTTP: the path every OpenTelemetry SDK and collector is pointed at.
	// Mounted at the root (not under /api/v1) because exporters take a base URL
	// and append /v1/traces themselves.
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		handleOTLPTraces(w, r, "")
	})
	mux.HandleFunc("/otel/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		handleOTLPTraces(w, r, "")
	})
	// ---- agent interface (MCP) and its authorization server ----
	mux.HandleFunc("/mcp", handleMCPHTTP)
	mux.HandleFunc("/.well-known/oauth-authorization-server", handleOAuthMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource", handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-protected-resource/", handleProtectedResourceMetadata)
	mux.HandleFunc("/oauth/register", handleOAuthRegister)
	mux.HandleFunc("/oauth/authorize", handleOAuthAuthorize)
	mux.HandleFunc("/oauth/token", handleOAuthToken)
	mux.HandleFunc("/oauth/revoke", handleOAuthRevoke)

	// ---- credentials ----
	mux.HandleFunc("/api/v1/keys", handleKeys)
	mux.HandleFunc("/api/v1/keys/", handleKeys)
	mux.HandleFunc("/api/v1/whoami", handleWhoAmI)

	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		handleProjects(w, r, "")
	})
	mux.HandleFunc("/api/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
		// parts[0] = "projects", parts[1] = pid, etc
		if len(parts) < 2 {
			errJSON(w, 404, "not found")
			return
		}
		projectID := parts[1]
		rest := parts[2:]
		switch {
		case len(rest) == 0:
			if r.Method != http.MethodGet {
				errJSON(w, 405, "method not allowed")
				return
			}
			handleProjectOne(w, r, "", projectID)
		case len(rest) == 1 && rest[0] == "spans:ingest":
			if r.Method != http.MethodPost {
				errJSON(w, 405, "method not allowed")
				return
			}
			handleIngest(w, r, projectID)
		case len(rest) == 3 && rest[0] == "otel" && rest[1] == "v1" && rest[2] == "traces":
			handleOTLPTraces(w, r, projectID)
		case len(rest) == 1 && rest[0] == "traces":
			handleTraces(w, r, projectID)
		case len(rest) == 2 && rest[0] == "traces":
			handleTraceDetail(w, r, projectID, rest[1])
		case len(rest) == 3 && rest[0] == "traces" && rest[2] == "feedback":
			handleFeedback(w, r, projectID, rest[1])
		case len(rest) == 1 && rest[0] == "dashboard":
			handleDashboard(w, r, projectID)
		case len(rest) == 1 && rest[0] == "datasets":
			handleDatasets(w, r, projectID)
		case len(rest) == 1 && rest[0] == "prompts":
			handlePrompts(w, r, projectID)
		case len(rest) == 1 && rest[0] == "evaluations":
			handleStartEval(w, r, projectID)
		case len(rest) == 1 && rest[0] == "experiments":
			handleExperimentsList(w, r, projectID)
		default:
			errJSON(w, 404, "route not found: "+strings.Join(rest, "/"))
		}
	})
	mux.HandleFunc("/api/v1/scorers", func(w http.ResponseWriter, r *http.Request) {
		handleScorers(w, r)
	})
	mux.HandleFunc("/api/v1/datasets/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/datasets/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			errJSON(w, 404, "not found")
			return
		}
		dsID := parts[0]
		switch {
		case len(parts) == 1:
			handleDatasetOne(w, r, "", dsID)
		case len(parts) == 2 && parts[1] == "records":
			handleAddRecord(w, r, "", dsID)
		case len(parts) == 2 && parts[1] == "records:from_trace":
			handleRecordFromTrace(w, r, "", dsID)
		default:
			errJSON(w, 404, "route not found")
		}
	})
	mux.HandleFunc("/api/v1/experiments/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/experiments/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			errJSON(w, 404, "not found")
			return
		}
		expID := parts[0]
		switch {
		case len(parts) == 1:
			handleExperimentDetail(w, r, "", expID)
		case len(parts) == 2 && parts[1] == "compare":
			handleCompare(w, r, "", expID)
		default:
			errJSON(w, 404, "route not found")
		}
	})
	mux.HandleFunc("/api/v1/worker_callback/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/worker_callback/"), "/")
		if len(parts) != 1 || parts[0] == "" {
			errJSON(w, 404, "bad path")
			return
		}
		handleWorkerCallback(w, r, "", parts[0])
	})

	return mux
}
