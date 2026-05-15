package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestAttachTerminalAndWait_ExitCode verifies the happy path: server sends a
// terminal_exit frame and the client returns the carried exit code.
func TestAttachTerminalAndWait_ExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		// Read the join_terminal frame so the client's WriteMessage doesn't race.
		_, _, _ = conn.ReadMessage()

		// Simulate the relayLLM "already exited" path: send terminal_joined
		// then terminal_exit with exit code 42.
		_ = conn.WriteJSON(map[string]interface{}{"type": "terminal_joined", "state": "stopped"})
		_ = conn.WriteJSON(map[string]interface{}{"type": "terminal_exit", "exitCode": 42})
	}))
	defer server.Close()

	client := NewLLMClient(server.URL, "", "")
	code, err := client.AttachTerminalAndWait("11111111-2222-3333-4444-555555555555", 5*time.Second)
	if err != nil {
		t.Fatalf("AttachTerminalAndWait: %v", err)
	}
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

// TestAttachTerminalAndWait_NotFound verifies the "session lost" path: the
// server returns a terminal-not-found error frame.
func TestAttachTerminalAndWait_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]interface{}{
			"type":    "error",
			"message": "terminal not found: abc",
		})
	}))
	defer server.Close()

	client := NewLLMClient(server.URL, "", "")
	code, err := client.AttachTerminalAndWait("11111111-2222-3333-4444-555555555555", 5*time.Second)
	if code != ExitCodeSessionLost {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeSessionLost)
	}
	if err == nil || !strings.Contains(err.Error(), "session lost") {
		t.Fatalf("err = %v, want 'session lost'", err)
	}
}

// TestAttachTerminalAndWait_Timeout verifies that the client stops waiting
// when the deadline elapses and reports ExitCodeTimeout.
func TestAttachTerminalAndWait_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		// Read once to swallow the join frame, then idle.
		_, _, _ = conn.ReadMessage()
		// Block until client disconnects.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	client := NewLLMClient(server.URL, "", "")
	start := time.Now()
	code, err := client.AttachTerminalAndWait("11111111-2222-3333-4444-555555555555", 150*time.Millisecond)
	elapsed := time.Since(start)
	if code != ExitCodeTimeout {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeTimeout)
	}
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("waited too long: %s", elapsed)
	}
}

// TestCreateTerminal_PayloadShape verifies the wire shape sent by the
// scheduler matches what relayLLM's POST /api/terminals expects.
func TestCreateTerminal_PayloadShape(t *testing.T) {
	var got map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":         "deadbeef-dead-beef-dead-beefdeadbeef",
			"templateId": "shell",
			"name":       "test",
			"directory":  "/tmp",
			"state":      "running",
		})
	}))
	defer server.Close()

	client := NewLLMClient(server.URL, "", "")
	resp, err := client.CreateTerminal(
		&Project{ID: "p1", Path: "/work"},
		"shell",
		"npm-test",
		[]string{"-c", "npm test"},
	)
	if err != nil {
		t.Fatalf("CreateTerminal: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("missing id in response")
	}
	if got["templateId"] != "shell" {
		t.Errorf("templateId = %v, want shell", got["templateId"])
	}
	if got["directory"] != "/work" {
		t.Errorf("directory = %v, want /work", got["directory"])
	}
	args, ok := got["extraArgs"].([]interface{})
	if !ok || len(args) != 2 || args[0] != "-c" || args[1] != "npm test" {
		t.Errorf("extraArgs = %v, want [-c, npm test]", got["extraArgs"])
	}
}
