package main

// OTLP/HTTP trace ingest.
//
// This is the endpoint a real OpenTelemetry SDK or collector points at, so it
// has to speak the wire format they actually emit: protobuf (the default for
// every SDK) and JSON (what curl and the collector use). The native
// spans:ingest endpoint stays as the fast path for our own client.
//
// Mapping rules follow Opik's OTel mapper: GenAI semantic conventions,
// OpenInference and the LangSmith-style llm.* attributes, in that order of
// preference.

import (
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// spanRow is the storage-shaped span shared by both ingest paths.
type spanRow struct {
	ID, TraceID, ParentID, Name, Type, Model string
	Input, Output, Error, Metadata           interface{}
	StartMS, EndMS, LatencyMS                int64
	PromptTokens, CompletionTokens           int
	CostUSD                                  float64
	Status                                   string
}

// insertSpanTx writes one span inside the caller's transaction. Callers own
// locking/serialisation so this function never opens its own connection.
func insertSpanTx(tx *sql.Tx, projectID string, s spanRow, now string) error {
	_, err := tx.Exec(`INSERT OR REPLACE INTO spans(id, trace_id, parent_id, name, type, model,
		input, output, error, metadata, start_ms, end_ms, latency_ms,
		prompt_tokens, completion_tokens, cost_usd, status, project_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.TraceID, nilIfEmpty(s.ParentID), s.Name, s.Type, nilIfEmpty(s.Model),
		jsonOrNull(s.Input), jsonOrNull(s.Output), jsonOrNull(s.Error), jsonOrNull(s.Metadata),
		s.StartMS, s.EndMS, s.LatencyMS, s.PromptTokens, s.CompletionTokens, s.CostUSD,
		s.Status, projectID, now)
	return err
}

// rollupTrace pushes child latency/errors up to root spans of the traces just
// written, so a trace row reflects the whole tree without a query per trace.
func rollupTraceTx(tx *sql.Tx) {
	tx.Exec(`UPDATE spans SET
		end_ms = (SELECT MAX(COALESCE(c.end_ms, c.start_ms)) FROM spans c WHERE c.trace_id = spans.trace_id AND c.parent_id IS NOT NULL),
		latency_ms = (SELECT MAX(COALESCE(c.end_ms, c.start_ms)) FROM spans c WHERE c.trace_id = spans.trace_id AND c.parent_id IS NOT NULL) - start_ms
		WHERE parent_id IS NULL AND latency_ms <= 100
		AND EXISTS (SELECT 1 FROM spans c WHERE c.trace_id = spans.trace_id AND c.parent_id IS NOT NULL)`)
	tx.Exec(`UPDATE spans SET status='error'
		WHERE parent_id IS NULL AND status='ok'
		AND EXISTS (SELECT 1 FROM spans c WHERE c.trace_id = spans.trace_id AND c.status='error')`)
}

// projectByName resolves a project for OTLP batches, creating it on first use
// the way Opik does (a collector should be able to point at an empty server).
func projectByName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	var id string
	err := db.QueryRow(`SELECT id FROM projects WHERE name=? ORDER BY created_at LIMIT 1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = newID()
	dbMu.Lock()
	_, err = db.Exec(`INSERT INTO projects(id, name, description, created_at) VALUES (?,?,?,?)`,
		id, name, "created by OTLP ingest", time.Now().UTC().Format(time.RFC3339Nano))
	dbMu.Unlock()
	if err != nil {
		// lost a race with a concurrent batch for the same new project
		if e := db.QueryRow(`SELECT id FROM projects WHERE name=? LIMIT 1`, name).Scan(&id); e == nil {
			return id, nil
		}
		return "", err
	}
	return id, nil
}

func anyValue(v *commonpb.AnyValue) interface{} {
	if v == nil {
		return nil
	}
	switch val := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_BoolValue:
		return val.BoolValue
	case *commonpb.AnyValue_IntValue:
		return val.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return val.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(val.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]interface{}, 0, len(val.ArrayValue.GetValues()))
		for _, item := range val.ArrayValue.GetValues() {
			out = append(out, anyValue(item))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]interface{}{}
		for _, kv := range val.KvlistValue.GetValues() {
			out[kv.Key] = anyValue(kv.Value)
		}
		return out
	}
	return nil
}

func attrsOf(kvs []*commonpb.KeyValue) map[string]interface{} {
	out := make(map[string]interface{}, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = anyValue(kv.Value)
	}
	return out
}

func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstInt(m map[string]interface{}, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case int64:
			return int(v)
		case int:
			return v
		case float64:
			return int(v)
		}
	}
	return 0
}

func firstAny(m map[string]interface{}, keys ...string) interface{} {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			return v
		}
	}
	return nil
}

// otelType maps OTel span kind conventions onto our span types.
func otelType(kind string, hasModel, isRoot bool) string {
	switch strings.ToUpper(kind) {
	case "LLM":
		return "llm"
	case "TOOL", "RETRIEVER":
		return "tool"
	case "AGENT", "CHAIN", "WORKFLOW":
		return "task"
	}
	if hasModel {
		return "llm"
	}
	if isRoot {
		return "task"
	}
	return "span"
}

func isZeroID(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// otelToRow converts one OTLP span into the storage row.
func otelToRow(sp *tracepb.Span, resource map[string]interface{}, scope string) spanRow {
	attrs := attrsOf(sp.GetAttributes())
	model := firstString(attrs, "gen_ai.request.model", "gen_ai.response.model",
		"llm.model_name", "ai.model.name", "model")
	kind := firstString(attrs, "openinference.span.kind", "gen_ai.operation.name", "span.kind")

	parent := ""
	if !isZeroID(sp.GetParentSpanId()) {
		parent = hex.EncodeToString(sp.GetParentSpanId())
	}
	isRoot := parent == ""
	start := int64(sp.GetStartTimeUnixNano() / 1e6)
	if start == 0 {
		start = time.Now().UnixMilli()
	}
	end := int64(sp.GetEndTimeUnixNano() / 1e6)
	if end == 0 {
		end = start
	}
	latency := end - start
	if latency < 0 {
		latency = 0
	}

	status := "ok"
	var errVal interface{}
	if sp.GetStatus() != nil && sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		status = "error"
		errVal = sp.GetStatus().GetMessage()
		if errVal == "" {
			errVal = "span status ERROR"
		}
	}
	if v := firstAny(attrs, "error", "exception.message"); v != nil && status == "ok" {
		status = "error"
		errVal = v
	}

	typ := otelType(kind, model != "", isRoot)
	pt := firstInt(attrs, "gen_ai.usage.input_tokens", "gen_ai.usage.prompt_tokens",
		"llm.token_count.prompt", "prompt_tokens", "input_tokens")
	ct := firstInt(attrs, "gen_ai.usage.output_tokens", "gen_ai.usage.completion_tokens",
		"llm.token_count.completion", "completion_tokens", "output_tokens")

	input := firstAny(attrs, "input", "gen_ai.prompt", "gen_ai.input.messages",
		"llm.input_messages", "input.value")
	output := firstAny(attrs, "output", "gen_ai.completion", "gen_ai.output.messages",
		"llm.output_messages", "output.value")

	cost := 0.0
	if typ == "llm" && model != "" {
		ip, op := priceFor(model)
		if pt == 0 {
			pt = estTokens(input)
		}
		if ct == 0 {
			ct = estTokens(output)
		}
		cost = float64(pt)/1000*ip + float64(ct)/1000*op
	}

	meta := map[string]interface{}{}
	if len(attrs) > 0 {
		meta["otel"] = attrs
	}
	if len(resource) > 0 {
		meta["resource"] = resource
	}
	if scope != "" {
		meta["scope"] = scope
	}
	if sp.GetKind() != tracepb.Span_SPAN_KIND_UNSPECIFIED {
		meta["otel_kind"] = sp.GetKind().String()
	}
	var metaVal interface{}
	if len(meta) > 0 {
		metaVal = meta
	}

	return spanRow{
		ID:       hex.EncodeToString(sp.GetSpanId()),
		TraceID:  hex.EncodeToString(sp.GetTraceId()),
		ParentID: parent, Name: sp.GetName(), Type: typ, Model: model,
		Input: input, Output: output, Error: errVal, Metadata: metaVal,
		StartMS: start, EndMS: end, LatencyMS: latency,
		PromptTokens: pt, CompletionTokens: ct, CostUSD: cost, Status: status,
	}
}

// handleOTLPTraces implements POST /v1/traces (OTLP/HTTP).
func handleOTLPTraces(w http.ResponseWriter, r *http.Request, pathProjectID string) {
	if r.Method != http.MethodPost {
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		errJSON(w, http.StatusBadRequest, "empty body")
		return
	}

	req := &collectortrace.ExportTraceServiceRequest{}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "json") {
		body, err = normalizeOTLPJSONIDs(body)
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid OTLP json: "+err.Error())
			return
		}
		if err := protojson.Unmarshal(body, req); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid OTLP json: "+err.Error())
			return
		}
	} else {
		if err := proto.Unmarshal(body, req); err != nil {
			errJSON(w, http.StatusBadRequest, "invalid OTLP protobuf: "+err.Error())
			return
		}
	}

	// OTLP treats an empty export as valid; answering 200 (and creating no
	// project) keeps collectors from retrying the same empty batch forever.
	total := 0
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			total += len(ss.GetSpans())
		}
	}
	if total == 0 {
		writeOTLPResponse(w, ct)
		return
	}

	projectID := pathProjectID
	if projectID == "" {
		name := r.Header.Get("project-name")
		if name == "" {
			name = r.Header.Get("X-Project-Name")
		}
		if projectID, err = projectByName(name); err != nil {
			errJSON(w, http.StatusInternalServerError, "resolve project: "+err.Error())
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	dbMu.Lock()
	tx, err := db.Begin()
	if err != nil {
		dbMu.Unlock()
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	inserted := 0
	for _, rs := range req.GetResourceSpans() {
		resource := attrsOf(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			scope := ss.GetScope().GetName()
			for _, sp := range ss.GetSpans() {
				row := otelToRow(sp, resource, scope)
				if row.ID == "" {
					continue // a span without an id cannot be addressed or deduped
				}
				if row.TraceID == "" {
					row.TraceID = row.ID
				}
				if err := insertSpanTx(tx, projectID, row, now); err != nil {
					dbMu.Unlock()
					errJSON(w, http.StatusInternalServerError, "span insert: "+err.Error())
					return
				}
				inserted++
			}
		}
	}
	rollupTraceTx(tx)
	if err := tx.Commit(); err != nil {
		dbMu.Unlock()
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	dbMu.Unlock()

	logf("otlp ingest", "project_id", projectID, "spans", inserted)
	writeOTLPResponse(w, ct)
}

// normalizeOTLPJSONIDs rewrites hex trace/span ids to base64 before protojson
// sees them.
//
// OTLP/JSON deliberately diverges from proto3 JSON: the spec says trace_id and
// span_id are hex, while protojson (correctly, for protobuf) expects base64.
// Feeding hex straight to protojson silently produces different ids — the
// content still parses, so the corruption is invisible until you look at a
// trace. Values that are already base64 are left untouched.
func normalizeOTLPJSONIDs(body []byte) ([]byte, error) {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	walkOTLPIDs(root)
	return json.Marshal(root)
}

func walkOTLPIDs(node interface{}) {
	switch v := node.(type) {
	case map[string]interface{}:
		for k, child := range v {
			switch k {
			case "traceId", "spanId", "parentSpanId":
				if s, ok := child.(string); ok {
					v[k] = hexIDToBase64(s)
					continue
				}
			}
			walkOTLPIDs(child)
		}
	case []interface{}:
		for _, child := range v {
			walkOTLPIDs(child)
		}
	}
}

func hexIDToBase64(s string) string {
	if len(s) != 16 && len(s) != 32 { // 8-byte span id, 16-byte trace id
		return s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return s // not hex: assume it is already base64
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// writeOTLPResponse answers in the encoding the exporter used; OTLP clients
// parse an empty ExportTraceServiceResponse as success.
func writeOTLPResponse(w http.ResponseWriter, contentType string) {
	if strings.Contains(contentType, "json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		return
	}
	out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
