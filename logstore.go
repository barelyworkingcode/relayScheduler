package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const maxLogEntries = 100

// LogStore persists task execution history to JSON files.
type LogStore struct {
	dir string
}

func NewLogStore(dir string) *LogStore {
	return &LogStore{dir: dir}
}

func (s *LogStore) logPath(projectID, taskID string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%s.json", projectID, taskID))
}

// Log appends an execution record to the log file.
func (s *LogStore) Log(projectID, taskID string, exec Execution) {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		slog.Error("failed to create log directory", "error", err)
		return
	}

	history := s.Load(projectID, taskID)

	// Prepend new entry (newest first).
	history = append([]Execution{exec}, history...)

	// Truncate to max entries.
	if len(history) > maxLogEntries {
		history = history[:maxLogEntries]
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		slog.Error("failed to marshal log", "error", err)
		return
	}

	path := s.logPath(projectID, taskID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		slog.Error("failed to write log", "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Error("failed to rename log", "error", err)
	}
}

// Load reads the execution history for a task.
func (s *LogStore) Load(projectID, taskID string) []Execution {
	data, err := os.ReadFile(s.logPath(projectID, taskID))
	if err != nil {
		return []Execution{}
	}

	var history []Execution
	if err := json.Unmarshal(data, &history); err != nil {
		return []Execution{}
	}
	return history
}
