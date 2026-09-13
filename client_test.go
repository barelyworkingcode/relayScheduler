package main

import (
	"encoding/json"
	"errors"
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
			t.Errorf("upgrade: %v", err)
			return
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

	client := NewRelayClient(server.URL, "", "")
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
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]interface{}{
			"type":    "error",
			"message": "terminal not found: abc",
		})
	}))
	defer server.Close()

	client := NewRelayClient(server.URL, "", "")
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
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		// Swallow the join frame, then idle until the client disconnects.
		_, _, _ = conn.ReadMessage()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	client := NewRelayClient(server.URL, "", "")
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

	client := NewRelayClient(server.URL, "", "")
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
	if got["projectId"] != "p1" {
		t.Errorf("projectId = %v, want p1", got["projectId"])
	}
	args, ok := got["extraArgs"].([]interface{})
	if !ok || len(args) != 2 || args[0] != "-c" || args[1] != "npm test" {
		t.Errorf("extraArgs = %v, want [-c, npm test]", got["extraArgs"])
	}
}

// chatServer upgrades one WS connection, records the two frames the client
// sends (join_session, send_message), then writes frames back.
func chatServer(t *testing.T, received *[]map[string]string, frames ...map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for range 2 {
			var msg map[string]string
			if err := conn.ReadJSON(&msg); err != nil {
				t.Errorf("read client frame: %v", err)
				return
			}
			*received = append(*received, msg)
		}
		for _, f := range frames {
			_ = conn.WriteJSON(f)
		}
		if len(frames) == 0 {
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, _ = conn.ReadMessage()
		}
	}))
}

func textDelta(sessionID, deltaType, text string) map[string]interface{} {
	return map[string]interface{}{
		"type":      "llm_event",
		"sessionId": sessionID,
		"event":     map[string]interface{}{"delta": map[string]string{"type": deltaType, "text": text}},
	}
}

func TestRunChatAndWait_AccumulatesOwnSessionText(t *testing.T) {
	var received []map[string]string
	server := chatServer(t, &received,
		textDelta("other-session", "text_delta", "wrong session"),
		textDelta("s1", "thinking_delta", "not user-visible"),
		textDelta("s1", "text_delta", "Hello, "),
		textDelta("s1", "text_delta", "world"),
		map[string]interface{}{"type": "stats_update", "sessionId": "s1", "stats": map[string]int{"inputTokens": 10, "outputTokens": 5}},
		map[string]interface{}{"type": "message_complete", "sessionId": "s1"},
	)
	defer server.Close()

	result, err := NewRelayClient(server.URL, "", "").RunChatAndWait("s1", "say hello", 5*time.Second)
	if err != nil {
		t.Fatalf("RunChatAndWait: %v", err)
	}
	if result.Response != "Hello, world" {
		t.Errorf("response = %q, want %q", result.Response, "Hello, world")
	}
	if result.Stats.InputTokens != 10 || result.Stats.OutputTokens != 5 {
		t.Errorf("stats = %+v, want 10 in / 5 out", result.Stats)
	}
	if len(received) != 2 || received[0]["type"] != "join_session" || received[1]["type"] != "send_message" || received[1]["text"] != "say hello" {
		t.Errorf("client frames = %v, want join_session then send_message", received)
	}
}

func TestRunChatAndWait_ErrorFrame(t *testing.T) {
	var received []map[string]string
	server := chatServer(t, &received,
		map[string]interface{}{"type": "error", "sessionId": "s1", "message": "model not loaded"},
	)
	defer server.Close()

	_, err := NewRelayClient(server.URL, "", "").RunChatAndWait("s1", "hi", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "model not loaded") {
		t.Fatalf("err = %v, want 'model not loaded'", err)
	}
}

func TestRunChatAndWait_Timeout(t *testing.T) {
	var received []map[string]string
	server := chatServer(t, &received)
	defer server.Close()

	_, err := NewRelayClient(server.URL, "", "").RunChatAndWait("s1", "hi", 150*time.Millisecond)
	if !errors.Is(err, ErrChatTimeout) {
		t.Fatalf("err = %v, want ErrChatTimeout", err)
	}
}
