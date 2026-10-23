package main

// MCP Streamable HTTP transport (spec revision 2025-06-18).
//
// One endpoint, `POST /mcp`, carries JSON-RPC messages from remote hosts. The
// transport is deliberately stateless: no session id is issued, so any instance
// behind a load balancer can serve any request, and the only state that matters
// is the bearer credential. Responses are `application/json` (the spec allows a
// server to answer without opening an SSE stream), and notifications get 202.
//
// Authorization is not re-implemented here: `authenticate` has already resolved
// the bearer token to a principal and put it in the request context, and
// mcpDispatch enforces per-tool scopes. Tool calls then forward the caller's own
// token to the API, so MCP can never exceed what the credential allows.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const mcpMaxBody = 4 << 20

func handleMCPHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		handleMCPPost(w, r)
	case http.MethodGet, http.MethodDelete:
		// No server-initiated stream and no sessions to terminate. The spec
		// allows 405 with an Allow header for exactly this server shape.
		w.Header().Set("Allow", "POST")
		errJSON(w, http.StatusMethodNotAllowed,
			"this MCP endpoint is stateless: POST JSON-RPC to /mcp (no SSE stream, no sessions)")
	default:
		w.Header().Set("Allow", "POST")
		errJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// checkMCPOrigin rejects cross-site browser origins. Without this, a malicious
// page could drive a localhost MCP server with the user's ambient credentials
// (DNS-rebinding class of attack; the MCP spec requires Origin validation).
func checkMCPOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil // native MCP clients do not send Origin
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errors.New("invalid Origin header")
	}
	base, err := url.Parse(issuer())
	if err == nil && strings.EqualFold(u.Host, base.Host) {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil // any loopback origin is the same trust zone as the server
	}
	return errors.New("origin not allowed: " + origin)
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]"
}

func handleMCPPost(w http.ResponseWriter, r *http.Request) {
	if err := checkMCPOrigin(r); err != nil {
		errJSON(w, http.StatusForbidden, err.Error())
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !mcpSupportedVersions[v] {
		errJSON(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version: "+v)
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		errJSON(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		errJSON(w, http.StatusBadRequest, "empty request body")
		return
	}

	// The caller's credential travels with the request so tool calls act as the
	// caller, not as the server.
	ctx := withBearer(r.Context(), bearerToken(r))
	w.Header().Set("Cache-Control", "no-store")

	// Batch (JSON array) or single message.
	if body[0] == '[' {
		var batch []mcpRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, http.StatusBadRequest, mcpError(nil, -32700, "parse error: "+err.Error()))
			return
		}
		if len(batch) == 0 {
			errJSON(w, http.StatusBadRequest, "empty batch")
			return
		}
		out := make([]map[string]interface{}, 0, len(batch))
		for _, req := range batch {
			if len(req.ID) == 0 {
				mcpDispatch(ctx, req) // notifications: handle, never answer
				continue
			}
			if resp := mcpDispatch(ctx, req); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, mcpError(nil, -32700, "parse error: "+err.Error()))
		return
	}
	if len(req.ID) == 0 {
		mcpDispatch(ctx, req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, mcpDispatch(ctx, req))
}
