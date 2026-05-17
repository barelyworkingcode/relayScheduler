package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

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
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Token string `json:"token"`
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

// GetProject fetches a project from relay's HTTP API. The scheduler needs
// this because relayLLM is a pure execution engine: it expects callers to
// pass `directory` and `mcpToken` explicitly, so the scheduler must resolve
// them from relay first (the same shape Eve uses).
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
		"mcpToken":  project.Token,
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

func (c *LLMClient) SendMessage(sessionID, text string) (*MessageResponse, error) {
	payload, _ := json.Marshal(map[string]string{"text": text})

	req, err := c.newRequest(http.MethodPost,
		fmt.Sprintf("/api/sessions/%s/message", sessionID),
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("send message failed (%d): %s", resp.StatusCode, body)
	}

	var result MessageResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
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
