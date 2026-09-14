package main

import (
	"encoding/json"
	"log/slog"
)

// Manifest is the wire shape relayScheduler declares to relay: the subset of
// relay/internal/bridge/manifest.go this service uses (relay is a separate Go
// module, so the type is mirrored). relay also accepts status and action
// declarations; the scheduler declares neither.
type Manifest struct {
	Routes []string `json:"routes"`
}

// registerManifestRequest mirrors RegisterManifestRequest in
// relay/internal/bridge/manifest.go.
type registerManifestRequest struct {
	ServiceID      string   `json:"serviceId"`
	Manifest       Manifest `json:"manifest"`
	InternalSocket string   `json:"internalSocket"`
	InternalToken  string   `json:"internalToken"`
}

// buildManifest declares the routes relay's front-door dispatcher forwards to
// this service. "/api/tasks" (exact) + "/api/tasks/" (prefix) cover the task
// API; "/ws/tasks" carries the lifecycle event stream eve subscribes to. It
// can't be "/ws": relayLLM already claims that and relay rejects duplicate
// routes.
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
// this service. Without a launch identity (standalone) it is a no-op: a
// tokenless bridge request from an unbound process is not a service to relay.
//
// Failure is logged and swallowed: the listener is already up, so losing
// relay dispatch is a partial degradation, not a reason to exit.
func maybeRegisterManifest(identity *launchIdentity, internalSocket, internalToken string) {
	if identity == nil {
		slog.Info("standalone mode — skipping manifest registration")
		return
	}
	bridgeSocket, serviceID := identity.bridgeSocket, identity.serviceID

	manifest := buildManifest()
	args, err := json.Marshal(registerManifestRequest{
		// relay accepts RegisterManifest only under the id bound at Hello.
		ServiceID:      serviceID,
		Manifest:       manifest,
		InternalSocket: internalSocket,
		InternalToken:  internalToken,
	})
	if err != nil {
		slog.Error("marshal manifest registration failed", "error", err)
		return
	}
	if err := sendBridgeRequest(bridgeSocket, reqRegisterManifest, args); err != nil {
		slog.Error("manifest registration failed; running without relay dispatch", "error", err)
		return
	}
	slog.Info("manifest registered with relay",
		"serviceId", serviceID,
		"internalSocket", internalSocket,
		"routes", len(manifest.Routes))
}
