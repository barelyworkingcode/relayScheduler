package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// newTestScheduler has no relay client, so tests must never let a task fire.
func newTestScheduler(t *testing.T) (*Scheduler, *TaskStore) {
	t.Helper()
	dir := t.TempDir()
	store := NewTaskStore(dir)
	logStore := NewLogStore(filepath.Join(dir, "task-logs"))
	return NewScheduler(nil, store, logStore, NewHub(store)), store
}

func TestCheckAndFire_MissedOnceTaskIsDisabled(t *testing.T) {
	s, store := newTestScheduler(t)
	at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	task, err := store.Create(Task{
		Name:      "one-shot",
		ProjectID: "p1",
		Prompt:    "hi",
		Enabled:   true,
		Schedule:  json.RawMessage(`{"type":"once","at":"` + at + `"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.LoadAllTasks(); err != nil {
		t.Fatalf("LoadAllTasks: %v", err)
	}
	if _, ok := s.tasks[task.ID]; !ok {
		t.Fatal("past once task was not scheduled on load, so catch-up could never fire it")
	}

	s.checkAndFireTasks()

	if _, ok := s.running[task.ID]; ok {
		t.Fatal("missed once task fired with catch-up disabled")
	}
	if _, ok := s.tasks[task.ID]; ok {
		t.Error("missed once task is still scheduled")
	}
	got, err := store.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Enabled {
		t.Error("missed once task is still enabled")
	}
}

func TestCheckAndFire_MissedRecurringTaskIsRescheduled(t *testing.T) {
	s, store := newTestScheduler(t)
	task, err := store.Create(Task{
		Name:      "every 15",
		ProjectID: "p1",
		Prompt:    "hi",
		Enabled:   true,
		Schedule:  json.RawMessage(`{"type":"interval","minutes":15}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.ScheduleTask(*task)
	s.tasks[task.ID].nextRun = time.Now().Add(-time.Hour)

	s.checkAndFireTasks()

	if _, ok := s.running[task.ID]; ok {
		t.Fatal("missed task fired with catch-up disabled")
	}
	st, ok := s.tasks[task.ID]
	if !ok {
		t.Fatal("missed recurring task dropped from the schedule")
	}
	if !st.nextRun.After(time.Now()) {
		t.Errorf("nextRun = %s, want a future time", st.nextRun)
	}
}
