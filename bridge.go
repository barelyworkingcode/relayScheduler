package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// Minimal client for relay's bridge Unix socket. relayScheduler makes one call,
// RegisterManifest, so relay's front-door dispatcher routes /api/tasks* and
// /ws/tasks to this process. Wire format mirrors relay/internal/bridge:
// newline-delimited JSON, one request, one response.
//
// The types are mirrored rather than imported because relay is a separate Go
// module and the surface needed here is tiny. Same pattern as
// ../relayLLM/relay_bridge_client.go.

const (
	relayBridgeTimeout = 5 * time.Second

	// Env vars relay injects into every spawned service. Mirror
	// relay/internal/bridge/types.go.
	envBridgeSocket = "RELAY_BRIDGE_SOCKET"
	envServiceID    = "RELAY_SERVICE_ID"

	// envServiceToken is the full-access service token that authenticates
	// bridge calls. It is NOT a project token and must never reach a spawned
	// child. relay still injects the pre-rename envServiceTokenLegacy alongside
	// it; drop the fallback once relay stops.
	envServiceToken       = "RELAY_SERVICE_TOKEN"
	envServiceTokenLegacy = "RELAY_MCP_TOKEN"

	// Must stay in sync with relay/internal/bridge/types.go.
	reqRegisterManifest = "RegisterManifest"
	respError           = "Error"
)

type relayBridgeRequest struct {
	Type      string          `json:"type"`
	Token     string          `json:"token,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type relayBridgeResponse struct {
	Type    string `json:"type"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func serviceToken() string {
	if t := os.Getenv(envServiceToken); t != "" {
		return t
	}
	return os.Getenv(envServiceTokenLegacy)
}

// sendBridgeRequest dials relay's bridge, writes one request, and reads one
// response. A bridge Error reply is returned as an error.
func sendBridgeRequest(socketPath, reqType string, args json.RawMessage) error {
	token := serviceToken()
	if token == "" {
		return fmt.Errorf("%s not set in environment", envServiceToken)
	}

	payload, err := json.Marshal(relayBridgeRequest{Type: reqType, Token: token, Arguments: args})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	conn, err := net.DialTimeout("unix", socketPath, relayBridgeTimeout)
	if err != nil {
		return fmt.Errorf("dial relay bridge at %s: %w", socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(relayBridgeTimeout))

	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("write to relay bridge: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read from relay bridge: %w", err)
		}
		return fmt.Errorf("relay bridge closed connection without responding")
	}

	var resp relayBridgeResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("parse relay response: %w", err)
	}
	if resp.Type == respError {
		return fmt.Errorf("relay bridge error (code %d): %s", resp.Code, resp.Message)
	}
	return nil
}
