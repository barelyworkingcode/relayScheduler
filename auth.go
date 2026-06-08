package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

// generateBearerToken returns a random 32-byte hex token. Used to auto-generate
// the internal bearer when one isn't supplied via flag/env so the listener is
// never unauthenticated. Mirrors ../relayLLM/auth.go.
func generateBearerToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// bearerAuth wraps an http.Handler with bearer-token authentication.
//
// If token is empty the middleware is a pass-through (standalone/dev on
// loopback). When set, every request — including the /ws/tasks WebSocket
// upgrade — must carry `Authorization: Bearer <token>` or it is rejected with
// 401 before any handler runs (so the WS upgrade never registers a hub client
// for an unauthenticated caller). Under relay, relay strips the inbound frontend
// bearer and injects this service-declared token when proxying. Comparison uses
// crypto/subtle.ConstantTimeCompare. Mirrors ../relayLLM/auth.go.
func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			slog.Debug("rejecting request: missing bearer header",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := []byte(strings.TrimSpace(header[len(prefix):]))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			slog.Warn("rejecting request: bad bearer token",
				"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
