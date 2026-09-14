package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// Service half of relay's launch-identity protocol (../relay/docs/launch-identity.md).
// Mirrors relay/internal/bridge/launch.go; relay is a separate Go module.

const (
	envLaunchFD = "RELAY_LAUNCH_FD"
	launchFD    = 3

	launchSecretLen = 64
	helloTimeout    = 10 * time.Second

	reqHello = "Hello"
	respOK   = "OK"
)

// removedCredentialEnv names relay no longer sets. They are unset from this
// process's environment under relay so nothing read later can pick up a
// stale value inherited from relay's own environment.
var removedCredentialEnv = []string{
	"RELAY_SERVICE_TOKEN",
	"RELAY_MCP_TOKEN",
	"RELAY_FRONTEND_TOKEN",
}

// launchIdentity is what a successful Hello establishes: relay recognises
// this exact process as serviceID. It holds no secret.
type launchIdentity struct {
	serviceID    string
	bridgeSocket string
	relayPID     int
}

// bootstrapLaunchIdentity must run before anything that spawns a process or
// calls relay. It returns nil with no error when RELAY_LAUNCH_FD is unset
// (standalone). When it is set, every failure is an error the caller must
// exit on: relay believes it launched this process, so degrading to a
// relay-less mode is never correct.
func bootstrapLaunchIdentity() (*launchIdentity, error) {
	rawFD, launched := os.LookupEnv(envLaunchFD)
	if !launched {
		return nil, nil
	}
	os.Unsetenv(envLaunchFD)
	for _, name := range removedCredentialEnv {
		os.Unsetenv(name)
	}

	fd, err := strconv.Atoi(rawFD)
	if err != nil || fd != launchFD {
		return nil, fmt.Errorf("%s=%q: want %d", envLaunchFD, rawFD, launchFD)
	}
	f := os.NewFile(uintptr(fd), "relay-launch")
	if f == nil {
		return nil, fmt.Errorf("%s: descriptor %d is not open", envLaunchFD, fd)
	}
	secret, err := readLaunchSecret(f)
	if err != nil {
		return nil, err
	}

	bridgeSocket := os.Getenv(envBridgeSocket)
	serviceID := os.Getenv(envServiceID)
	if bridgeSocket == "" || serviceID == "" {
		return nil, fmt.Errorf("%s and %s are required when %s is set", envBridgeSocket, envServiceID, envLaunchFD)
	}
	relayPID, err := sendHello(bridgeSocket, serviceID, secret)
	if err != nil {
		return nil, err
	}
	return &launchIdentity{serviceID: serviceID, bridgeSocket: bridgeSocket, relayPID: relayPID}, nil
}

// readLaunchSecret reads f to EOF and closes it on every path, so no later
// spawn can inherit the descriptor. Errors never include the bytes read.
func readLaunchSecret(f *os.File) (string, error) {
	// One byte past the secret's length, so an over-long pipe is detected
	// rather than truncated into a valid-looking secret.
	buf, readErr := io.ReadAll(io.LimitReader(f, launchSecretLen+1))
	closeErr := f.Close()
	if readErr != nil {
		return "", fmt.Errorf("read launch secret: %w", readErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close launch fd: %w", closeErr)
	}
	if !isLaunchSecret(buf) {
		return "", errors.New("launch secret is not 64 lowercase hex characters")
	}
	return string(buf), nil
}

func isLaunchSecret(b []byte) bool {
	if len(b) != launchSecretLen {
		return false
	}
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type helloRequest struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

type helloResponse struct {
	Type    string `json:"type"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Data    struct {
		ServiceID string `json:"service_id"`
		RelayPID  int    `json:"relay_pid"`
	} `json:"data"`
}

// sendHello presents the launch secret on a fresh bridge connection and
// returns relay's pid. The identity relay binds belongs to this process, not
// the connection, so the connection is closed afterwards.
func sendHello(socketPath, name, secret string) (int, error) {
	conn, err := net.DialTimeout("unix", socketPath, relayBridgeTimeout)
	if err != nil {
		return 0, fmt.Errorf("hello: dial relay bridge at %s: %w", socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(helloTimeout))

	payload, err := json.Marshal(helloRequest{Type: reqHello, Name: name, Token: secret})
	if err != nil {
		return 0, fmt.Errorf("hello: marshal: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return 0, fmt.Errorf("hello: write: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return 0, fmt.Errorf("hello: read: %w", err)
		}
		return 0, errors.New("hello: relay bridge closed connection without responding")
	}
	var resp helloResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return 0, fmt.Errorf("hello: parse response: %w", err)
	}
	switch {
	case resp.Type == respError:
		return 0, fmt.Errorf("hello refused by relay (code %d): %s", resp.Code, resp.Message)
	case resp.Type != respOK:
		return 0, fmt.Errorf("hello: unexpected response type %q", resp.Type)
	case resp.Data.ServiceID != name:
		return 0, fmt.Errorf("hello: relay recognised service %q, sent %q", resp.Data.ServiceID, name)
	case resp.Data.RelayPID <= 0:
		return 0, fmt.Errorf("hello: invalid relay_pid %d", resp.Data.RelayPID)
	}
	return resp.Data.RelayPID, nil
}
