package main

import (
	"encoding/json"
	"log/slog"
	"os"
)

// Manifest is the wire shape relayScheduler declares to relay. Must stay
// field-compatible with relay/bridge/manifest.go (a separate Go module, so the
// types are intentionally mirrored here). Same pattern as ../relayLLM/manifest.go.
type Manifest struct {
	Routes  []string     `json:"routes"`
	Status  *StatusDecl  `json:"status,omitempty"`
	Actions []ActionDecl `json:"actions,omitempty"`
}

type StatusDecl struct {
	Path string `json:"path"`
}

type ActionDecl struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	PathTemplate string `json:"pathTemplate"`
	ForEach      string `json:"forEach,omitempty"`
}

// registerManifestRequest is the Arguments payload for the RegisterManifest
// bridge call. Field names mirror relay/bridge/manifest.go's
// RegisterManifestRequest.
type registerManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// buildManifest declares the routes relay's front-door dispatcher forwards to
// this service. "/api/tasks" (exact) + "/api/tasks/" (prefix) cover the task
// CRUD + run + history API; "/ws/tasks" carries the task-lifecycle event stream
// eve subscribes to. A distinct "/ws/tasks" path (not "/ws") is required:
// relayLLM already claims "/ws" and relay rejects duplicate routes.
func buildManifest() Manifest {
	return Manifest{
		Routes: []string{
			"/api/tasks",
			"/api/tasks/",
			"/ws/tasks",
		},
	}
}

// maybeRegisterManifest tells relay where to dispatch front-door traffic for
// this service. Standalone runs (no RELAY_BRIDGE_SOCKET set) are a clean no-op —
// direct clients still reach the listener.
//
// Failure is logged and swallowed: the listener is already up, so missing the
// relay-dispatch path is a partial degradation, not a hard error.
func maybeRegisterManifest(internalSocket, internalToken string) {
	if os.Getenv(envBridgeSocket) == "" {
		slog.Info("standalone mode — skipping manifest registration")
		return
	}
	serviceID := os.Getenv(envServiceID)
	if serviceID == "" {
		slog.Warn("bridge socket set but service ID missing — skipping manifest registration", "env", envServiceID)
		return
	}

	manifest := buildManifest()
	args, err := json.Marshal(registerManifestRequest{
		ServiceID:      serviceID,
		Manifest:       manifest,
		InternalSocket: internalSocket,
		InternalToken:  internalToken,
	})
	if err != nil {
		slog.Error("marshal manifest registration failed", "error", err)
		return
	}
	if _, err := sendBridgeRequest(reqRegisterManifest, args); err != nil {
		slog.Error("manifest registration failed; running without relay dispatch", "error", err)
		return
	}
	slog.Info("manifest registered with relay",
		"serviceId", serviceID,
		"internalSocket", internalSocket,
		"routes", len(manifest.Routes))
}
