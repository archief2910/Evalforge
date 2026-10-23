package main

// Tests for the OTLP/HTTP ingest contract. These drive the handler through
// httptest with a real (temporary) SQLite database, so they assert what an
// exporter actually observes: status, encoding, and the rows that land.

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func testServer(t *testing.T) {
	t.Helper()
	os.Setenv("DATA_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
}

func str(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func num(n int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: n}}
}

func sampleRequest(traceID, rootID, childID []byte) *collectortrace.ExportTraceServiceRequest {
	root := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            rootID,
		Name:              "agent.run",
		Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: 1_700_000_000_000_000_000,
		EndTimeUnixNano:   1_700_000_001_000_000_000,
	}
	child := &tracepb.Span{
		TraceId:           traceID,
		SpanId:            childID,
		ParentSpanId:      rootID,
		Name:              "openai.chat",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: 1_700_000_000_100_000_000,
		EndTimeUnixNano:   1_700_000_000_900_000_000,
		Attributes: []*commonpb.KeyValue{
			{Key: "gen_ai.request.model", Value: str("gpt-4o")},
			{Key: "gen_ai.usage.input_tokens", Value: num(100)},
			{Key: "gen_ai.usage.output_tokens", Value: num(50)},
			{Key: "input", Value: str("hello")},
			{Key: "output", Value: str("hi there")},
		},
	}
	return &collectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: str("test-svc")},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "opentelemetry.instrumentation.openai"},
				Spans: []*tracepb.Span{root, child},
			}},
		}},
	}
}

func postOTLP(t *testing.T, body []byte, contentType string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handleOTLPTraces(rec, req, "")
	return rec
}

func TestOTLPProtobufIngest(t *testing.T) {
	testServer(t)
	traceID := bytes.Repeat([]byte{0x11}, 16)
	rootID := bytes.Repeat([]byte{0x01}, 8)
	childID := bytes.Repeat([]byte{0x02}, 8)

	body, err := proto.Marshal(sampleRequest(traceID, rootID, childID))
	if err != nil {
		t.Fatal(err)
	}
	rec := postOTLP(t, body, "application/x-protobuf", map[string]string{"project-name": "otlp-svc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("response content-type = %q", got)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("spans = %d, want 2", n)
	}

	wantTrace := hex.EncodeToString(traceID)
	var gotTrace, model, typ, status, parent string
	var pt, ct int
	var cost float64
	if err := db.QueryRow(`SELECT trace_id, COALESCE(model,''), type, status, COALESCE(parent_id,''),
		prompt_tokens, completion_tokens, cost_usd FROM spans WHERE id=?`,
		hex.EncodeToString(childID)).Scan(&gotTrace, &model, &typ, &status, &parent, &pt, &ct, &cost); err != nil {
		t.Fatal(err)
	}
	if gotTrace != wantTrace {
		t.Errorf("trace_id = %q, want %q", gotTrace, wantTrace)
	}
	if model != "gpt-4o" {
		t.Errorf("model = %q", model)
	}
	if typ != "llm" {
		t.Errorf("type = %q, want llm", typ)
	}
	if pt != 100 || ct != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", pt, ct)
	}
	// gpt-4o: 0.0025/1k in, 0.01/1k out
	wantCost := 100.0/1000*0.0025 + 50.0/1000*0.01
	if diff := cost - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %f, want %f", cost, wantCost)
	}
	if status != "ok" {
		t.Errorf("status = %q", status)
	}

	// Root latency is rolled up from the child and the tree is addressable.
	var rootTrace, rootParent string
	var latency int64
	if err := db.QueryRow(`SELECT trace_id, COALESCE(parent_id,''), latency_ms FROM spans WHERE id=?`,
		hex.EncodeToString(rootID)).Scan(&rootTrace, &rootParent, &latency); err != nil {
		t.Fatal(err)
	}
	if rootTrace != wantTrace || rootParent != "" {
		t.Errorf("root trace=%q parent=%q", rootTrace, rootParent)
	}
	// OTLP spans carry real end times, so the root keeps its own duration
	// (the placeholder-rollup path only applies to roots that end at +100ms).
	if latency != 1000 {
		t.Errorf("root latency = %d, want 1000", latency)
	}
	var childLatency int64
	if err := db.QueryRow(`SELECT latency_ms FROM spans WHERE id=?`, hex.EncodeToString(childID)).Scan(&childLatency); err != nil {
		t.Fatal(err)
	}
	if childLatency != 800 {
		t.Errorf("child latency = %d, want 800", childLatency)
	}

	// Retried batch: same span ids must not duplicate.
	rec = postOTLP(t, body, "application/x-protobuf", map[string]string{"project-name": "otlp-svc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d", rec.Code)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM spans`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("spans after retry = %d, want idempotent 2", n)
	}
}

func TestOTLPJSONIngestAndEmptyBatch(t *testing.T) {
	testServer(t)
	traceID := bytes.Repeat([]byte{0x22}, 16)
	body, err := protojson.Marshal(sampleRequest(traceID, bytes.Repeat([]byte{0x03}, 8), bytes.Repeat([]byte{0x04}, 8)))
	if err != nil {
		t.Fatal(err)
	}
	rec := postOTLP(t, body, "application/json", map[string]string{"project-name": "json-svc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	// OTLP/JSON uses hex ids where protojson expects base64; the stored id must
	// still be the hex id the exporter sent.
	var storedTrace string
	if err := db.QueryRow(`SELECT trace_id FROM spans LIMIT 1`).Scan(&storedTrace); err != nil {
		t.Fatal(err)
	}
	if storedTrace != hex.EncodeToString(traceID) {
		t.Errorf("json trace_id = %q, want %q", storedTrace, hex.EncodeToString(traceID))
	}

	// Empty export: valid OTLP, 200, and no project should be created for it.
	empty := []byte(`{"resourceSpans":[]}`)
	rec = postOTLP(t, empty, "application/json", map[string]string{"project-name": "ghost"})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty status = %d", rec.Code)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE name='ghost'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("empty batch created a project")
	}
}

func TestOTLPRejectsGarbage(t *testing.T) {
	testServer(t)
	rec := postOTLP(t, []byte{0xff, 0xff, 0xff}, "application/x-protobuf", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	rec = postOTLP(t, []byte(`{"nope":`), "application/json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("json status = %d, want 400", rec.Code)
	}
}
