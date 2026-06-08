package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Minimal client for relay's bridge Unix socket. relayScheduler uses it for a
// single call: registering its service manifest so relay's front-door
// dispatcher routes /api/tasks* (and the /ws/tasks event stream) to this
// process. Authenticates with the full-access service token relay injects via
// RELAY_SERVICE_TOKEN at spawn. Wire format mirrors relay/bridge — newline-
// delimited JSON, one request, one response.
//
// The types are intentionally mirrored (not imported) from relay/bridge: relay
// is a separate Go module and the surface we need is tiny. Same pattern as
// ../relayLLM/relay_bridge_client.go.

const (
	relayBridgeSocketName = "relay.sock"
	relayBridgeTimeout    = 5 * time.Second

	// Env vars relay injects into every spawned service. Mirror
	// relay/bridge/types.go + relay/service_registry.go.
	envBridgeSocket = "RELAY_BRIDGE_SOCKET"
	envServiceID    = "RELAY_SERVICE_ID"

	// envServiceToken is the full-access service token used to authenticate
	// bridge calls. It is NOT a project token and must never be injected into a
	// spawned child. envServiceTokenLegacy is the pre-rename name, accepted as a
	// fallback during the cross-repo rename window; drop once relay stops
	// setting it.
	envServiceToken       = "RELAY_SERVICE_TOKEN"
	envServiceTokenLegacy = "RELAY_MCP_TOKEN"

	// Bridge request/response type values. Must stay in sync with
	// relay/bridge/types.go.
	reqRegisterManifest = "RegisterManifest"
	respError           = "Error"
)

// relayBridgeRequest is the on-wire request envelope.
type relayBridgeRequest struct {
	Type      string          `json:"type"`
	Token     string          `json:"token,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// relayBridgeResponse is the on-wire response envelope.
type relayBridgeResponse struct {
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Code    int             `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// relayBridgeSocketPath returns the path where relay's bridge listens. Prefers
// RELAY_BRIDGE_SOCKET (set by relay at spawn) and falls back to the
// conventional location so direct/dev invocations still work without env setup.
func relayBridgeSocketPath() string {
	if p := os.Getenv(envBridgeSocket); p != "" {
		return p
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir, _ = os.UserHomeDir()
	}
	return filepath.Join(configDir, "relay", relayBridgeSocketName)
}

// serviceToken returns the full-access service token relay injected at spawn,
// preferring the current env name and falling back to the legacy name during
// the cross-repo rename window. Empty when this process was not spawned by
// relay (standalone/dev runs).
func serviceToken() string {
	if t := os.Getenv(envServiceToken); t != "" {
		return t
	}
	return os.Getenv(envServiceTokenLegacy)
}

// sendBridgeRequest dials relay's bridge socket, writes one request, reads one
// response, and returns the parsed envelope. Authentication is the service
// token relay issued at spawn (falling back to the legacy name).
func sendBridgeRequest(reqType string, args json.RawMessage) (relayBridgeResponse, error) {
	token := serviceToken()
	if token == "" {
		return relayBridgeResponse{}, fmt.Errorf("%s not set in environment (relay-managed callers require a service token)", envServiceToken)
	}

	payload, err := json.Marshal(relayBridgeRequest{
		Type:      reqType,
		Token:     token,
		Arguments: args,
	})
	if err != nil {
		return relayBridgeResponse{}, fmt.Errorf("marshal envelope: %w", err)
	}

	sockPath := relayBridgeSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, relayBridgeTimeout)
	if err != nil {
		return relayBridgeResponse{}, fmt.Errorf("dial relay bridge at %s: %w (is Relay tray app running?)", sockPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayBridgeTimeout))

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return relayBridgeResponse{}, fmt.Errorf("write to relay bridge: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return relayBridgeResponse{}, fmt.Errorf("read from relay bridge: %w", err)
		}
		return relayBridgeResponse{}, fmt.Errorf("relay bridge closed connection without responding")
	}

	var resp relayBridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return relayBridgeResponse{}, fmt.Errorf("parse relay response: %w", err)
	}
	if resp.Type == respError {
		return resp, fmt.Errorf("relay bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return resp, nil
}
