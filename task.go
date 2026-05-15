package main

import "encoding/json"

// Task viewer kinds. Frontends dispatch on these — they don't peek at
// SessionType or LastSessionID/LastTerminalID directly. The set is closed:
// new task types either collapse into one of these viewer shapes or add a
// new constant (a deliberate API change).
const (
	TaskViewInteractive = "interactive" // resumable LLM session, joinable
	TaskViewReadonly    = "readonly"    // captured byte stream, replay only
)

// Task.SessionType values.
const (
	SessionTypeChat = "headless"
	SessionTypePTY  = "pty"
)

// TaskView is the wire shape that abstracts chat vs PTY tasks for the
// frontend. Used for both the persisted descriptor (on Task.MarshalJSON,
// where RunID is the last-known run) and lifecycle broadcasts (where RunID
// is the run being announced).
type TaskView struct {
	Kind       string `json:"kind"`
	RunID      string `json:"runId,omitempty"`
	HasLastRun bool   `json:"hasLastRun,omitempty"`
}

// taskView returns the view descriptor for a task and the given runID. With
// runID == "" the function falls back to the task's last persisted run —
// the shape used when serializing a stored task. With runID != "" the
// returned view announces that specific run (HasLastRun is left false
// because broadcasts don't need the existence flag).
func taskView(t Task, runID string) TaskView {
	kind := TaskViewInteractive
	if t.SessionType == SessionTypePTY {
		kind = TaskViewReadonly
	}
	if runID != "" {
		return TaskView{Kind: kind, RunID: runID}
	}
	storedID := t.LastSessionID
	if t.SessionType == SessionTypePTY {
		storedID = t.LastTerminalID
	}
	return TaskView{Kind: kind, RunID: storedID, HasLastRun: storedID != ""}
}

// MarshalJSON augments the wire format with the derived `view` field.
// Storage round-trips cleanly: Task has no View struct field, so the
// reader silently drops it and the next write regenerates it.
func (t Task) MarshalJSON() ([]byte, error) {
	type alias Task
	return json.Marshal(struct {
		alias
		View TaskView `json:"view"`
	}{
		alias: alias(t),
		View:  taskView(t, ""),
	})
}

// Task defines a scheduled task. Two execution modes are supported:
//
//	SessionType == "headless" (default, or empty for legacy tasks):
//	    The scheduler creates a headless LLM session and sends Prompt.
//	    Result is the chat response; transcript persists on relayLLM and is
//	    joined by Eve via LastSessionID.
//
//	SessionType == "pty":
//	    The scheduler launches a terminal session from TemplateID with
//	    ExtraArgs appended. Result is the process exit code and the raw
//	    byte stream (head + tail log). Eve replays via LastTerminalID.
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
	LastStatus    string `json:"lastStatus,omitempty"` // "success", "error", "timeout"
	LastSessionID string `json:"lastSessionId,omitempty"`
	CatchUp       bool   `json:"catchUp"` // Run missed executions after sleep/wake

	// PTY-mode fields. Zero-valued for legacy headless tasks; no migration
	// needed. SessionType is "headless" (or "") for chat, "pty" for terminal.
	SessionType        string   `json:"sessionType,omitempty"`
	TemplateID         string   `json:"templateId,omitempty"`
	ExtraArgs          []string `json:"extraArgs,omitempty"`
	Directory          string   `json:"directory,omitempty"` // optional override of project.Path
	MaxDurationSeconds int      `json:"maxDurationSeconds,omitempty"`
	LastTerminalID     string   `json:"lastTerminalId,omitempty"`
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

// Execution records a single task run. Both chat and PTY executions use
// the same record; the populated fields tell them apart (SessionID for
// chat, TerminalID + ExitCode for PTY).
type Execution struct {
	TaskID      string        `json:"taskId"`
	TaskName    string        `json:"taskName"`
	ProjectID   string        `json:"projectId"`
	SessionID   string        `json:"sessionId,omitempty"`
	StartedAt   string        `json:"startedAt"`
	CompletedAt string        `json:"completedAt,omitempty"`
	Status      string        `json:"status"` // "running", "success", "error", "timeout"
	Response    string        `json:"response,omitempty"`
	Error       string        `json:"error,omitempty"`
	Stats       *SessionStats `json:"stats,omitempty"`

	// PTY-mode fields. Pointer ExitCode so 0 (real success) is
	// distinguishable from "no exit code captured".
	TerminalID string `json:"terminalId,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
}
