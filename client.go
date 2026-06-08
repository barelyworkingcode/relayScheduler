package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ErrChatTimeout is returned by RunChatAndWait when a chat turn does not
// complete within the caller's wall-clock cap. Distinct so the scheduler can
// record a "timeout" status (mirroring the PTY path) rather than a hard error.
var ErrChatTimeout = errors.New("chat response timeout")

// Synthetic exit codes the scheduler emits when the PTY didn't actually
// produce one. Real process exits use 0..255; negative values are reserved
// for scheduler-side conditions. Documented in plans/well-lets-think-more-rippling-dongarra.md.
const (
	ExitCodeSessionLost  = -1 // WS dropped, relayLLM restart, or "terminal not found"
	ExitCodeTimeout      = -2 // MaxDurationSeconds elapsed
	ExitCodeCreateFailed = -3 // POST /api/terminals failed
)

// LLMClient communicates with relay's HTTP API. After the front-door
// migration, the scheduler talks only to relay (over a Unix socket); relay
// reverse-proxies session traffic to relayLLM internally.
type LLMClient struct {
	baseURL    string
	token      string
	http       *http.Client
	socketPath string // empty when using TCP; non-empty when dialing a Unix socket
}

// Project mirrors the snake_case shape relay returns from /api/projects/{id}.
// Only the fields the scheduler needs are decoded.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// No Token: relay brokers project tokens now (it strips the token from
	// /api/projects responses). The scheduler references projects by id and
	// relayLLM resolves the scoped token from relay's bridge by projectId.
}

type SessionResponse struct {
	SessionID string `json:"sessionId"`
	Model     string `json:"model"`
}

type MessageResponse struct {
	Response string       `json:"response"`
	Stats    SessionStats `json:"stats"`
}

type SessionStats struct {
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CostUsd             float64 `json:"costUsd"`
}

// NewLLMClient builds a client for relay's HTTP API.
//
// When socketPath is non-empty, the transport dials that Unix socket and the
// baseURL host is purely cosmetic — required by the URL parser but ignored by
// the dialer. Token is sent as a bearer header on every request.
func NewLLMClient(baseURL, socketPath, token string) *LLMClient {
	transport := &http.Transport{}
	if socketPath != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}
		// The host portion of the URL is irrelevant for Unix-socket transport
		// but must parse cleanly; pin it to a synthetic value.
		baseURL = "http://relay-frontend.localsocket"
	}
	return &LLMClient{
		baseURL:    baseURL,
		token:      token,
		http:       &http.Client{Timeout: 10 * time.Minute, Transport: transport},
		socketPath: socketPath,
	}
}

func (c *LLMClient) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// GetProject fetches a project from relay's HTTP API. The scheduler needs the
// project's id and path to tell relayLLM which project to run under (by id) and
// where (directory). The token is brokered by relay — relayLLM resolves the
// scoped token from relay's bridge by projectId — so the scheduler never sees it.
func (c *LLMClient) GetProject(projectID string) (*Project, error) {
	req, err := c.newRequest(http.MethodGet, "/api/projects/"+projectID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get project failed (%d): %s", resp.StatusCode, body)
	}

	var p Project
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *LLMClient) CreateSession(project *Project, model, name string) (*SessionResponse, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"projectId": project.ID,
		"directory": project.Path,
		"model":     model,
		"name":      name,
		"settings":  map[string]bool{"headless": true},
	})

	req, err := c.newRequest(http.MethodPost, "/api/sessions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create session failed (%d): %s", resp.StatusCode, body)
	}

	var session SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return nil, err
	}
	return &session, nil
}

// RunChatAndWait drives one chat turn over the WebSocket and blocks until the
// turn completes, errors, the provider dies, or the timeout elapses. It joins
// the session, sends the prompt, and accumulates the streamed reply text + stats
// from the broadcast event frames (the same frames relayLLM sends every viewer).
//
// Why WS instead of POST /api/sessions/{id}/message: that synchronous endpoint
// caps a response at relayLLM's 5-minute collector.Wait and returns 504 past
// it, which the scheduler recorded as a spurious "error" even though slow
// local-model runs were still progressing and finished fine server-side. The WS
// event stream has no such cap, so the only bound is this caller's timeout and a
// long run reports its real outcome. Mirrors AttachTerminalAndWait for chat.
func (c *LLMClient) RunChatAndWait(sessionID, prompt string, timeout time.Duration) (*MessageResponse, error) {
	conn, err := c.dialWS("/ws")
	if err != nil {
		return nil, fmt.Errorf("dial ws: %w", err)
	}
	defer conn.Close()

	// Join first so we are a registered viewer before generation starts.
	// relayLLM reads a connection's frames sequentially on one goroutine, so the
	// join is processed (viewer added) before send_message kicks off the turn.
	join, _ := json.Marshal(map[string]string{"type": "join_session", "sessionId": sessionID})
	if err := conn.WriteMessage(websocket.TextMessage, join); err != nil {
		return nil, fmt.Errorf("send join_session: %w", err)
	}
	send, _ := json.Marshal(map[string]string{"type": "send_message", "sessionId": sessionID, "text": prompt})
	if err := conn.WriteMessage(websocket.TextMessage, send); err != nil {
		return nil, fmt.Errorf("send_message: %w", err)
	}

	var text strings.Builder
	var stats SessionStats
	deadline := time.Now().Add(timeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrChatTimeout
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil, ErrChatTimeout
			}
			return nil, fmt.Errorf("ws read: %w", err)
		}

		var frame struct {
			Type      string          `json:"type"`
			SessionID string          `json:"sessionId"`
			Event     json.RawMessage `json:"event"`
			Stats     *SessionStats   `json:"stats"`
			Message   string          `json:"message"`
		}
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue // unparseable frame — ignore and keep reading.
		}
		// The /ws stream is shared across sessions; ignore other sessions'
		// frames (session-less frames like terminal events have no sessionId).
		if frame.SessionID != "" && frame.SessionID != sessionID {
			continue
		}

		switch frame.Type {
		case "llm_event":
			// Accumulate user-visible text only (text_delta), matching the
			// synchronous ResponseCollector — thinking/tool blocks are excluded.
			var ev struct {
				Delta *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal(frame.Event, &ev) == nil && ev.Delta != nil && ev.Delta.Type == "text_delta" {
				text.WriteString(ev.Delta.Text)
			}
		case "stats_update":
			if frame.Stats != nil {
				stats = *frame.Stats
			}
		case "message_complete":
			return &MessageResponse{Response: text.String(), Stats: stats}, nil
		case "process_exited":
			return nil, fmt.Errorf("provider process exited unexpectedly")
		case "error":
			return nil, fmt.Errorf("%s", frame.Message)
		}
	}
}

// fireAndForget issues a bodiless request and discards the response.
// Best-effort: any error is swallowed because these endpoints are cleanup
// operations on relayLLM where failure isn't actionable from here.
func (c *LLMClient) fireAndForget(method, path string) {
	req, err := c.newRequest(method, path, nil)
	if err != nil {
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// StopGeneration aborts an in-flight LLM response without ending the session.
func (c *LLMClient) StopGeneration(sessionID string) {
	c.fireAndForget(http.MethodPost, fmt.Sprintf("/api/sessions/%s/stop", sessionID))
}

// DeleteSession removes the session from memory and disk on relayLLM.
// Uses POST /api/sessions/{id}/delete rather than the DELETE verb — the
// DELETE handler only ends the session and keeps the file on disk.
func (c *LLMClient) DeleteSession(sessionID string) {
	c.fireAndForget(http.MethodPost, fmt.Sprintf("/api/sessions/%s/delete", sessionID))
}

// --- Terminal/PTY methods ---

// TerminalResponse mirrors the response from POST /api/terminals.
type TerminalResponse struct {
	ID         string `json:"id"`
	TemplateID string `json:"templateId"`
	Name       string `json:"name"`
	Directory  string `json:"directory"`
	State      string `json:"state"`
}

// CreateTerminal launches a PTY session on relayLLM with the given template
// and per-task extra args. The terminal's directory defaults to project.Path.
// On success, the returned ID is what callers persist on the Task and use
// for AttachTerminalAndWait / GetTerminalLog / CloseTerminal.
func (c *LLMClient) CreateTerminal(project *Project, templateID, name string, extraArgs []string) (*TerminalResponse, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"templateId": templateID,
		"name":       name,
		"directory":  project.Path,
		"projectId":  project.ID, // lets relay issue a project-scoped token for the PTY
		"cols":       120,
		"rows":       30,
		"extraArgs":  extraArgs,
	})

	req, err := c.newRequest(http.MethodPost, "/api/terminals", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create terminal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create terminal failed (%d): %s", resp.StatusCode, body)
	}

	var out TerminalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTerminalLog returns the stitched head+tail bytes of the PTY's output.
// Works even after the in-memory session has been evicted, as long as the
// log files have not been swept.
func (c *LLMClient) GetTerminalLog(terminalID string) ([]byte, error) {
	req, err := c.newRequest(http.MethodGet, fmt.Sprintf("/api/terminals/%s/log", terminalID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get terminal log: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get terminal log failed (%d): %s", resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// CloseTerminal kills a PTY session. Best-effort: cleanup is non-critical
// because the relayLLM idle timeout would eventually GC it.
func (c *LLMClient) CloseTerminal(terminalID string) {
	c.fireAndForget(http.MethodDelete, "/api/terminals/"+terminalID)
}

// AttachTerminalAndWait opens a WebSocket to relay's /ws endpoint, joins the
// terminal, and blocks until either:
//
//   - terminal_exit arrives → returns the real exit code (0..255).
//   - timeout elapses → returns ExitCodeTimeout (-2).
//   - WS error / close / "terminal not found" → returns ExitCodeSessionLost (-1).
//
// The scheduler does not buffer terminal_output frames — they are persisted
// on the relayLLM side by terminalLogger. This WS attach exists purely to
// learn the exit code reliably.
func (c *LLMClient) AttachTerminalAndWait(terminalID string, timeout time.Duration) (int, error) {
	conn, err := c.dialWS("/ws")
	if err != nil {
		return ExitCodeSessionLost, fmt.Errorf("dial ws: %w", err)
	}
	defer conn.Close()

	joinMsg, _ := json.Marshal(map[string]string{
		"type":       "join_terminal",
		"terminalId": terminalID,
	})
	if err := conn.WriteMessage(websocket.TextMessage, joinMsg); err != nil {
		return ExitCodeSessionLost, fmt.Errorf("send join_terminal: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ExitCodeTimeout, fmt.Errorf("timeout after %s", timeout)
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return ExitCodeSessionLost, fmt.Errorf("set read deadline: %w", err)
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			// net.Error.Timeout() distinguishes our wall-clock cap from a
			// real connection drop. Either way the run is over.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return ExitCodeTimeout, fmt.Errorf("timeout after %s", timeout)
			}
			return ExitCodeSessionLost, fmt.Errorf("ws read: %w", err)
		}

		var frame struct {
			Type     string `json:"type"`
			ExitCode int    `json:"exitCode"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(msg, &frame); err != nil {
			continue // unparseable frame — ignore and keep reading.
		}

		switch frame.Type {
		case "terminal_exit":
			return frame.ExitCode, nil
		case "error":
			// "terminal not found" means the in-memory session is gone
			// (relayLLM restart or idle GC) before we could attach.
			if strings.Contains(frame.Message, "terminal not found") {
				return ExitCodeSessionLost, fmt.Errorf("session lost: %s", frame.Message)
			}
			return ExitCodeSessionLost, fmt.Errorf("ws error: %s", frame.Message)
		}
		// terminal_joined, terminal_output, etc. — keep reading.
	}
}

// dialWS opens a WebSocket to relay's /ws endpoint, using the same Unix
// socket the HTTP client uses when configured, and sending the bearer token.
func (c *LLMClient) dialWS(path string) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	if c.socketPath != "" {
		dialer.NetDial = func(_, _ string) (net.Conn, error) {
			return (&net.Dialer{}).Dial("unix", c.socketPath)
		}
	}
	// http://… → ws://… and https://… → wss://…
	wsURL := strings.Replace(c.baseURL, "http", "ws", 1) + path
	headers := http.Header{}
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}
	conn, _, err := dialer.Dial(wsURL, headers)
	return conn, err
}
