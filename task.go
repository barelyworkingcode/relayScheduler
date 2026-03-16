package main

import "encoding/json"

// Task defines a scheduled LLM task.
type Task struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Prompt    string          `json:"prompt"`
	Schedule  json.RawMessage `json:"schedule"`
	Enabled   bool            `json:"enabled"`
	Model     string          `json:"model,omitempty"`
	ProjectID string          `json:"projectId"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt,omitempty"`
	LastRun   string          `json:"lastRun,omitempty"`
	LastStatus    string `json:"lastStatus,omitempty"` // "success", "error", ""
	LastSessionID string `json:"lastSessionId,omitempty"`
}

// Schedule types parsed from the schedule JSON.

type DailySchedule struct {
	Type string `json:"type"`
	Time string `json:"time"` // "HH:MM"
}

type HourlySchedule struct {
	Type   string `json:"type"`
	Minute int    `json:"minute"`
}

type IntervalSchedule struct {
	Type    string `json:"type"`
	Minutes int    `json:"minutes"`
}

type WeeklySchedule struct {
	Type string `json:"type"`
	Day  string `json:"day"`  // "monday", "tuesday", etc.
	Time string `json:"time"` // "HH:MM"
}

type CronSchedule struct {
	Type       string `json:"type"`
	Expression string `json:"expression"`
}

type OnceSchedule struct {
	Type string `json:"type"`
	At   string `json:"at"` // ISO8601
}

type OnDemandSchedule struct {
	Type string `json:"type"`
}

// Execution records a single task run.
type Execution struct {
	TaskID      string        `json:"taskId"`
	TaskName    string        `json:"taskName"`
	ProjectID   string        `json:"projectId"`
	SessionID   string        `json:"sessionId,omitempty"`
	StartedAt   string        `json:"startedAt"`
	CompletedAt string        `json:"completedAt,omitempty"`
	Status      string        `json:"status"` // "running", "success", "error"
	Response    string        `json:"response,omitempty"`
	Error       string        `json:"error,omitempty"`
	Stats       *SessionStats `json:"stats,omitempty"`
}
