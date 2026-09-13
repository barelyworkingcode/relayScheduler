package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
)

// generateBearerToken returns a random 32-byte hex token for the inbound
// listener when none is supplied. Mirrors ../relayLLM/auth.go.
func generateBearerToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// bearerAuth rejects any request without `Authorization: Bearer <token>`
// before a handler runs, so an unauthenticated /ws/tasks upgrade never
// registers a hub client. Under relay, the front-door dispatcher replaces the
// caller's bearer with this token when proxying. Mirrors ../relayLLM/auth.go.
func bearerAuth(token string, next http.Handler) http.Handler {
	// An empty token would match an empty bearer and leave the listener open.
	if token == "" {
		panic("bearerAuth: empty token")
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
