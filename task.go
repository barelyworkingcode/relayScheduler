package main

import "encoding/json"

// TaskFile is the structure of a .tasks.json file.
type TaskFile struct {
	Version int    `json:"version"`
	Tasks   []Task `json:"tasks"`
}

// Task defines a scheduled LLM task.
type Task struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Prompt    string          `json:"prompt"`
	Schedule  json.RawMessage `json:"schedule"`
	Enabled   bool            `json:"enabled"`
	Model     string          `json:"model,omitempty"`
	Args      []string        `json:"args,omitempty"`
	CreatedAt string          `json:"createdAt"`
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

// Execution records a single task run.
type Execution struct {
	TaskID      string        `json:"taskId"`
	TaskName    string        `json:"taskName"`
	ProjectID   string        `json:"projectId"`
	ProjectName string        `json:"projectName"`
	StartedAt   string        `json:"startedAt"`
	CompletedAt string        `json:"completedAt,omitempty"`
	Status      string        `json:"status"` // "running", "success", "error"
	Response    string        `json:"response,omitempty"`
	Error       string        `json:"error,omitempty"`
	Stats       *SessionStats `json:"stats,omitempty"`
}
