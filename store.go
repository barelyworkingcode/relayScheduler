package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TaskStore provides mutex-protected CRUD over a single tasks.json file.
type TaskStore struct {
	mu   sync.Mutex
	path string
}

func NewTaskStore(dataDir string) *TaskStore {
	return &TaskStore{path: filepath.Join(dataDir, "tasks.json")}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Load reads all tasks from disk.
func (s *TaskStore) Load() ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *TaskStore) readLocked() ([]Task, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Task{}, nil
		}
		return nil, err
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parse tasks.json: %w", err)
	}
	return tasks, nil
}

func (s *TaskStore) writeLocked(tasks []Task) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: write to temp file then rename, so a crash mid-write
	// cannot corrupt tasks.json.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ListByProject returns tasks filtered by project ID.
func (s *TaskStore) ListByProject(projectID string) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	var result []Task
	for _, t := range all {
		if t.ProjectID == projectID {
			result = append(result, t)
		}
	}
	return result, nil
}

// Get returns a single task by ID.
func (s *TaskStore) Get(id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.ID == id {
			return &t, nil
		}
	}
	return nil, nil
}

// Create adds a new task with a generated ID and timestamps.
func (s *TaskStore) Create(task Task) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	task.ID = generateID()
	task.CreatedAt = now
	task.UpdatedAt = now

	tasks = append(tasks, task)
	if err := s.writeLocked(tasks); err != nil {
		return nil, err
	}
	slog.Info("created task", "id", task.ID, "name", task.Name)
	return &task, nil
}

// Update replaces a task by ID. Returns the updated task or nil if not found.
func (s *TaskStore) Update(id string, updated Task) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		return nil, err
	}

	for i, t := range tasks {
		if t.ID == id {
			updated.ID = id
			updated.CreatedAt = t.CreatedAt
			updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if updated.LastRun == "" {
				updated.LastRun = t.LastRun
			}
			if updated.LastStatus == "" {
				updated.LastStatus = t.LastStatus
			}
			if updated.LastSessionID == "" {
				updated.LastSessionID = t.LastSessionID
			}
			if updated.LastTerminalID == "" {
				updated.LastTerminalID = t.LastTerminalID
			}
			tasks[i] = updated
			if err := s.writeLocked(tasks); err != nil {
				return nil, err
			}
			slog.Info("updated task", "id", id, "name", updated.Name)
			return &updated, nil
		}
	}
	return nil, nil
}

// Delete removes a task by ID. Returns true if found and deleted.
func (s *TaskStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		return false, err
	}

	for i, t := range tasks {
		if t.ID == id {
			tasks = append(tasks[:i], tasks[i+1:]...)
			if err := s.writeLocked(tasks); err != nil {
				return false, err
			}
			slog.Info("deleted task", "id", id)
			return true, nil
		}
	}
	return false, nil
}

// DeleteByProject removes all tasks for a project. Returns count deleted.
func (s *TaskStore) DeleteByProject(projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		return 0, err
	}

	kept := tasks[:0]
	deleted := 0
	for _, t := range tasks {
		if t.ProjectID == projectID {
			deleted++
		} else {
			kept = append(kept, t)
		}
	}

	if deleted == 0 {
		return 0, nil
	}

	if err := s.writeLocked(kept); err != nil {
		return 0, err
	}
	slog.Info("deleted tasks by project", "projectId", projectID, "count", deleted)
	return deleted, nil
}

// updateTask is a shared helper for lock/read/find/mutate/write operations.
func (s *TaskStore) updateTask(id string, fn func(*Task)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks, err := s.readLocked()
	if err != nil {
		slog.Error("failed to read tasks for update", "id", id, "error", err)
		return
	}

	for i, t := range tasks {
		if t.ID == id {
			fn(&tasks[i])
			tasks[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := s.writeLocked(tasks); err != nil {
				slog.Error("failed to write task update", "id", id, "error", err)
			}
			return
		}
	}
}

// SetLastRun updates the LastRun and LastStatus for a task (called after execution).
func (s *TaskStore) SetLastRun(id, status string) {
	s.updateTask(id, func(t *Task) {
		t.LastRun = time.Now().UTC().Format(time.RFC3339)
		t.LastStatus = status
	})
}

// SetLastSessionID updates the LastSessionID for a task.
func (s *TaskStore) SetLastSessionID(id, sessionID string) {
	s.updateTask(id, func(t *Task) {
		t.LastSessionID = sessionID
	})
}

// SetLastTerminalID updates the LastTerminalID for a PTY task. Eve uses
// this to wire the "View Last Run" click on a terminal task to the read-only
// PTY viewer.
func (s *TaskStore) SetLastTerminalID(id, terminalID string) {
	s.updateTask(id, func(t *Task) {
		t.LastTerminalID = terminalID
	})
}

// SetEnabled updates the Enabled flag for a task.
func (s *TaskStore) SetEnabled(id string, enabled bool) {
	s.updateTask(id, func(t *Task) {
		t.Enabled = enabled
	})
}
