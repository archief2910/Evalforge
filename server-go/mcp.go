package main

// MCP (Model Context Protocol) server.
//
// Opik ships an `opik-mcp` server so Claude Code / Cursor / VS Code can drive
// the platform, and an OAuth 2.1 authorization server so remote MCP clients can
// authenticate. EvalForge does the same thing at its own scale: the evaluation
// platform becomes tools an agent can call, so "what regressed in the last
// experiment?" is answerable from the editor instead of a browser.
//
// Two transports, one implementation:
//
//	stdio  `evalforge --mcp`     what a local MCP host launches; credential
//	                             comes from MCP_API_KEY.
//	HTTP   `POST /mcp`           Streamable HTTP for remote clients; credential
//	                             is the OAuth bearer token (see auth.go), or an
//	                             API key when AUTH_MODE is off.
//
// Design choice: the tool layer is a *client* of the public HTTP API, exactly
// like the Python SDK. It never opens the database, so it cannot diverge from
// the API's semantics and it cannot bypass authorization: the caller's own
// credential is forwarded on every tool call, which means a project-scoped key
// stays project-scoped inside the agent too.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpServerName      = "evalforge"
	mcpServerVersion   = "1.1.0"
)

// Versions we can speak; we echo the client's if it is one of these.
var mcpSupportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpTool struct {
	Name        string
	Description string
	Schema      map[string]interface{}
	// ScopeMin is the credential scope a caller must hold. Reads need "read",
	// anything that mutates state needs "write".
	ScopeMin string
	Handler  func(ctx context.Context, args map[string]interface{}) (interface{}, error)
}

// mcpCallParams is the tools/call payload.
type mcpCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

var mcpBaseURL = "http://localhost:8000"

// mcpAPIKey is the credential used by the stdio transport; HTTP requests carry
// their own bearer token in the context instead.
var mcpAPIKey = ""

type bearerCtxKey struct{}

func withBearer(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, bearerCtxKey{}, tok)
}

func bearerFrom(ctx context.Context) string {
	if v, ok := ctx.Value(bearerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func mcpLog(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[mcp] "+format+"\n", args...)
}

func hasFlag(flag string) bool {
	for _, a := range os.Args[1:] {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue returns the argument following flag, if present.
func flagValue(flag string) (string, bool) {
	for i, a := range os.Args[1:] {
		if a == flag && i+2 <= len(os.Args)-1 {
			return os.Args[i+2], true
		}
	}
	return "", false
}

// runMCP is the process entry point for --mcp. It never returns.
func runMCP() {
	if v := os.Getenv("MCP_API_BASE"); v != "" {
		mcpBaseURL = strings.TrimRight(v, "/")
	}
	mcpAPIKey = os.Getenv("MCP_API_KEY")
	mcpLog("serving MCP on stdio; API=%s protocol=%s auth=%s",
		mcpBaseURL, mcpProtocolVersion, map[bool]string{true: "api key", false: "none"}[mcpAPIKey != ""])

	// stdout carries protocol messages only; anything else must go to stderr.
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	ctx := context.Background()

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			mcpLog("parse error: %v", err)
			_ = enc.Encode(mcpError(nil, -32700, "parse error: "+err.Error()))
			continue
		}
		if len(req.ID) == 0 {
			// Notification (e.g. notifications/initialized): no response.
			continue
		}
		resp := mcpDispatch(ctx, req)
		if err := enc.Encode(resp); err != nil {
			mcpLog("write error: %v", err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		mcpLog("stdin closed: %v", err)
	}
}

// mcpDispatch handles one JSON-RPC message. It returns nil for notifications
// so both transports can skip the write.
func mcpDispatch(ctx context.Context, req mcpRequest) map[string]interface{} {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpProtocolVersion
		if mcpSupportedVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return mcpResult(req.ID, map[string]interface{}{
			"protocolVersion": version,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{"listChanged": false},
			},
			"serverInfo":   map[string]interface{}{"name": mcpServerName, "version": mcpServerVersion},
			"instructions": "EvalForge: inspect traces, curate datasets, run evaluations and compare experiments. Read tools need the read scope; add_feedback and run_evaluation need write.",
		})
	case "ping":
		return mcpResult(req.ID, map[string]interface{}{})
	case "notifications/initialized", "notifications/cancelled":
		return nil
	case "tools/list":
		tools := make([]map[string]interface{}, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.Schema,
				"annotations": map[string]interface{}{
					"readOnlyHint": t.ScopeMin == "read",
				},
			})
		}
		return mcpResult(req.ID, map[string]interface{}{"tools": tools})
	case "tools/call":
		var p mcpCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return mcpError(req.ID, -32602, "invalid params: "+err.Error())
		}
		tool := findMCPTool(p.Name)
		if tool == nil {
			return mcpError(req.ID, -32602, "unknown tool: "+p.Name)
		}
		if pr := principalFrom(ctx); pr != nil && !pr.can(tool.ScopeMin) {
			return mcpResult(req.ID, map[string]interface{}{
				"isError": true,
				"content": []map[string]interface{}{{"type": "text", "text": fmt.Sprintf(
					"insufficient scope: %s requires the %q scope; this credential has %q",
					tool.Name, tool.ScopeMin, joinScopes(pr.Scopes))}},
			})
		}
		if p.Arguments == nil {
			p.Arguments = map[string]interface{}{}
		}
		out, err := tool.Handler(ctx, p.Arguments)
		if err != nil {
			return mcpResult(req.ID, map[string]interface{}{
				"isError": true,
				"content": []map[string]interface{}{{"type": "text", "text": err.Error()}},
			})
		}
		text, _ := json.MarshalIndent(out, "", "  ")
		return mcpResult(req.ID, map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": string(text)}},
		})
	}
	return mcpError(req.ID, -32601, "method not found: "+req.Method)
}

func mcpResult(id json.RawMessage, result interface{}) map[string]interface{} {
	return map[string]interface{}{"jsonrpc": "2.0", "id": rawID(id), "result": result}
}

func mcpError(id json.RawMessage, code int, msg string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0", "id": rawID(id),
		"error": map[string]interface{}{"code": code, "message": msg},
	}
}

// rawID keeps the client's id type (string or number) intact.
func rawID(id json.RawMessage) interface{} {
	if len(id) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(id, &v); err != nil {
		return nil
	}
	return v
}

func findMCPTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

// ---------- HTTP client ----------

var mcpHTTP = &http.Client{Timeout: 60 * time.Second}

// mcpCall reaches the public API with the caller's credential, never with an
// implicit privileged one.
func mcpCall(ctx context.Context, method, path string, body interface{}) (interface{}, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, mcpBaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := bearerFrom(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if mcpAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+mcpAPIKey)
	}
	resp, err := mcpHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s %s: %w (is the API running at %s?)", method, path, err, mcpBaseURL)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var parsed interface{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parsed, nil
}

// ---------- argument helpers ----------

func argString(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func argInt(args map[string]interface{}, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func argStrings(args map[string]interface{}, key string) []string {
	var out []string
	if list, ok := args[key].([]interface{}); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func requireArg(args map[string]interface{}, key string) (string, error) {
	if v := argString(args, key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("missing required argument %q", key)
}

// resolveProject accepts either project_id or project_name, so an agent can say
// "Demo Support Agent" without a lookup round trip.
func resolveProject(ctx context.Context, args map[string]interface{}) (string, error) {
	if id := argString(args, "project_id"); id != "" {
		return id, nil
	}
	name := argString(args, "project_name")
	if name == "" {
		return "", fmt.Errorf("one of project_id or project_name is required")
	}
	raw, err := mcpCall(ctx, "GET", "/api/v1/projects", nil)
	if err != nil {
		return "", err
	}
	list, _ := raw.([]interface{})
	for _, item := range list {
		p, _ := item.(map[string]interface{})
		if pn, _ := p["name"].(string); strings.EqualFold(pn, name) {
			id, _ := p["id"].(string)
			return id, nil
		}
	}
	return "", fmt.Errorf("no project named %q", name)
}

// ---------- tools ----------

var mcpTools = []mcpTool{
	{
		Name:        "list_projects",
		Description: "List projects. A project groups traces, datasets, prompts and experiments.",
		Schema:      mcpObj(map[string]interface{}{}),
		ScopeMin:    "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return mcpCall(ctx, "GET", "/api/v1/projects", nil)
		},
	},

	{
		Name:        "search_traces",
		Description: "Search production traces (one row per trace, with aggregated model, cost, latency). Use this to find failures, slow calls or expensive runs.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
			"status":       schemaStr("Filter: ok or error."),
			"model":        schemaStr("Filter by model name used anywhere in the trace."),
			"search":       schemaStr("Substring match on input, output, span name or error."),
			"has_feedback": schemaBool("Only traces with human feedback."),
			"sort_by":      schemaEnum([]string{"start_ms", "latency_ms", "cost_usd"}),
			"sort_dir":     schemaEnum([]string{"desc", "asc"}),
			"limit":        schemaInt("Max rows (default 20, max 200)."),
			"offset":       schemaInt("Pagination offset."),
		}, "project_id", "project_name"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			q := []string{}
			add := func(k, v string) {
				if v != "" {
					q = append(q, k+"="+url.QueryEscape(v))
				}
			}
			add("status", argString(args, "status"))
			add("model", argString(args, "model"))
			add("search", argString(args, "search"))
			add("sort_by", argString(args, "sort_by"))
			add("sort_dir", argString(args, "sort_dir"))
			if v, ok := args["has_feedback"].(bool); ok && v {
				q = append(q, "has_feedback=true")
			}
			limit := argInt(args, "limit", 20)
			if limit > 200 {
				limit = 200
			}
			q = append(q, fmt.Sprintf("limit=%d&offset=%d", limit, argInt(args, "offset", 0)))
			return mcpCall(ctx, "GET", "/api/v1/projects/"+pid+"/traces?"+strings.Join(q, "&"), nil)
		},
	},
	{
		Name:        "get_trace",
		Description: "Fetch one trace as a nested span tree, including per-span tokens/cost and human feedback. Use span ids from here for feedback.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
			"trace_id":     schemaStr("Trace id (the root span id)."),
		}, "trace_id", "project_id", "project_name"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			tid, err := requireArg(args, "trace_id")
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/projects/"+pid+"/traces/"+tid, nil)
		},
	},
	{
		Name:        "get_dashboard",
		Description: "Project health: request count, error rate, p50/p95/p99 latency, token and cost totals, model breakdown, time series.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
			"hours":        schemaInt("Window in hours (default 168)."),
		}, "project_id", "project_name"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", fmt.Sprintf("/api/v1/projects/%s/dashboard?hours=%d", pid, argInt(args, "hours", 168)), nil)
		},
	},
	{
		Name:        "list_datasets",
		Description: "List datasets in a project (evaluation inputs).",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
		}, "project_id", "project_name"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/projects/"+pid+"/datasets", nil)
		},
	},
	{
		Name:        "get_dataset",
		Description: "Fetch a dataset and its records (input/expected pairs).",
		Schema: mcpObj(map[string]interface{}{
			"dataset_id": schemaStr("Dataset id from list_datasets."),
		}, "dataset_id"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			id, err := requireArg(args, "dataset_id")
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/datasets/"+id, nil)
		},
	},
	{
		Name:        "list_experiments",
		Description: "List evaluation experiments with status, run counts and mean scores.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
		}, "project_id", "project_name"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/projects/"+pid+"/experiments", nil)
		},
	},
	{
		Name:        "get_experiment",
		Description: "Fetch one experiment with per-case runs, scores, latencies and score provenance. Poll this while an experiment is running.",
		Schema: mcpObj(map[string]interface{}{
			"experiment_id": schemaStr("Experiment id from list_experiments or run_evaluation."),
		}, "experiment_id"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			id, err := requireArg(args, "experiment_id")
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/experiments/"+id, nil)
		},
	},
	{
		Name:        "compare_experiments",
		Description: "Compare an experiment against a baseline case-by-case: score deltas, improvements and regressions. This is how you answer 'did this change help?'.",
		Schema: mcpObj(map[string]interface{}{
			"experiment_id": schemaStr("Experiment under test."),
			"baseline_id":   schemaStr("Baseline experiment id."),
		}, "experiment_id", "baseline_id"),
		ScopeMin: "read",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			eid, err := requireArg(args, "experiment_id")
			if err != nil {
				return nil, err
			}
			bid, err := requireArg(args, "baseline_id")
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "GET", "/api/v1/experiments/"+eid+"/compare?baseline_id="+url.QueryEscape(bid), nil)
		},
	},
	{
		Name:        "add_feedback",
		Description: "Record human feedback on a trace (thumbs up/down plus a comment). Use after a review so the signal is visible in the UI and filters.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":   schemaStr("Project id (preferred)."),
			"project_name": schemaStr("Project name, used when project_id is absent."),
			"trace_id":     schemaStr("Trace id."),
			"kind":         schemaEnum([]string{"thumbs", "comment", "correction"}),
			"value":        schemaStr("up or down for thumbs; free text otherwise."),
			"comment":      schemaStr("Why the answer was right or wrong."),
		}, "trace_id", "kind", "value", "project_id", "project_name"),
		ScopeMin: "write",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			tid, err := requireArg(args, "trace_id")
			if err != nil {
				return nil, err
			}
			kind, err := requireArg(args, "kind")
			if err != nil {
				return nil, err
			}
			value, err := requireArg(args, "value")
			if err != nil {
				return nil, err
			}
			return mcpCall(ctx, "POST", "/api/v1/projects/"+pid+"/traces/"+tid+"/feedback",
				map[string]interface{}{"kind": kind, "value": value, "comment": argString(args, "comment")})
		},
	},
	{
		Name:        "run_evaluation",
		Description: "Start an evaluation of a dataset against a baseline. Returns an experiment id immediately; poll get_experiment until status is completed or failed.",
		Schema: mcpObj(map[string]interface{}{
			"project_id":             schemaStr("Project id (preferred)."),
			"project_name":           schemaStr("Project name, used when project_id is absent."),
			"dataset_id":             schemaStr("Dataset to evaluate."),
			"name":                   schemaStr("Experiment name (optional)."),
			"scorers":                schemaArr("Scorer names from GET /api/v1/scorers; defaults to exact_match, contains, factuality_judge."),
			"baseline_experiment_id": schemaStr("Optional baseline experiment id."),
		}, "dataset_id", "project_id", "project_name"),
		ScopeMin: "write",
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			pid, err := resolveProject(ctx, args)
			if err != nil {
				return nil, err
			}
			dsid, err := requireArg(args, "dataset_id")
			if err != nil {
				return nil, err
			}
			scorers := argStrings(args, "scorers")
			if len(scorers) == 0 {
				scorers = []string{"exact_match", "contains", "factuality_judge"}
			}
			name := argString(args, "name")
			if name == "" {
				name = "MCP evaluation " + time.Now().UTC().Format("2006-01-02 15:04")
			}
			body := map[string]interface{}{
				"dataset_id": dsid, "experiment_name": name, "scorer_names": scorers,
			}
			if b := argString(args, "baseline_experiment_id"); b != "" {
				body["baseline_experiment_id"] = b
			}
			return mcpCall(ctx, "POST", "/api/v1/projects/"+pid+"/evaluations", body)
		},
	},
}

// ---------- schema builders ----------

func mcpObj(props map[string]interface{}, required ...string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

func schemaStr(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func schemaInt(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func schemaArr(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type": "array", "description": desc,
		"items": map[string]interface{}{"type": "string"},
	}
}

func schemaBool(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func schemaEnum(values []string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "enum": values}
}
