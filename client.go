package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// LLMClient communicates with relay's HTTP API. After the front-door
// migration, the scheduler talks only to relay (over a Unix socket); relay
// reverse-proxies session traffic to relayLLM internally.
type LLMClient struct {
	baseURL string
	token   string
	http    *http.Client
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
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Minute, Transport: transport},
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

func (c *LLMClient) EndSession(sessionID string) {
	req, err := c.newRequest(http.MethodPost, fmt.Sprintf("/api/sessions/%s/stop", sessionID), nil)
	if err != nil {
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
