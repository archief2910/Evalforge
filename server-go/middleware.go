package main

// HTTP plumbing: correlation ids, panic recovery, access logging, CORS.
// These wrap the router so every handler gets them for free.

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

var reqSeq atomic.Uint64

// auditSlotType carries a *string the auth layer fills in, so the access log can
// name the principal without a second log line per request.
type auditSlotType struct{}

func setAuditOwner(ctx context.Context, owner string) {
	if slot, ok := ctx.Value(auditSlotType{}).(*string); ok {
		*slot = owner
	}
}

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

// logf keeps the call sites short while emitting structured events.
func logf(msg string, args ...any) { logger.Info(msg, args...) }

// statusRecorder captures the status code for the access log without changing
// handler behaviour.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic", "request_id", requestIDOf(r), "path", r.URL.Path, "panic", rec)
				// The header may already be written; errJSON on a hijacked
				// writer would double-write, so encode directly.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		var owner string
		r = r.WithContext(context.WithValue(r.Context(), auditSlotType{}, &owner))
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		slogArgs := []any{
			"request_id", requestIDOf(r),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if owner != "" {
			slogArgs = append(slogArgs, "owner", owner)
		}
		logger.Info("http", slogArgs...)
	})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-Id, project-name, Authorization, X-API-Key, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withMiddleware composes the chain outermost-first: CORS answers preflights
// before any logging, correlation ids precede everything that logs, and panic
// recovery sits inside requestID so a recovered panic still carries the id.
// `authenticate` wraps the router itself (see main), so a rejected request is
// still logged with its request id and duration.
func withMiddleware(next http.Handler) http.Handler {
	return cors(requestID(recoverPanic(accessLog(next))))
}

func requestIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}
