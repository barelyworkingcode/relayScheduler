package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LLMClient communicates with the relayLLM HTTP API.
type LLMClient struct {
	baseURL string
	http    *http.Client
}

type Project struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Model string `json:"model"`
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

func NewLLMClient(baseURL string) *LLMClient {
	return &LLMClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *LLMClient) ListProjects() ([]Project, error) {
	resp, err := c.http.Get(c.baseURL + "/api/projects")
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var projects []Project
	if err := json.Unmarshal(body, &projects); err != nil {
		return nil, fmt.Errorf("parse projects: %w", err)
	}
	return projects, nil
}

func (c *LLMClient) CreateSession(projectID, model string) (*SessionResponse, error) {
	payload, _ := json.Marshal(map[string]string{
		"projectId": projectID,
		"model":     model,
		"name":      "Scheduled Task",
	})

	resp, err := c.http.Post(c.baseURL+"/api/sessions", "application/json", bytes.NewReader(payload))
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

	resp, err := c.http.Post(
		fmt.Sprintf("%s/api/sessions/%s/message", c.baseURL, sessionID),
		"application/json",
		bytes.NewReader(payload),
	)
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
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/sessions/%s", c.baseURL, sessionID), nil)
	if err != nil {
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
