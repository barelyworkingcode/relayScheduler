package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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

func TestExecuteTask_ChatTaskWithoutModelFailsWithoutSession(t *testing.T) {
	var sessionPosts atomic.Int32
	frontDoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/p1":
			w.Write([]byte(`{"id":"p1","path":"/work"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			sessionPosts.Add(1)
			http.Error(w, "fake front door creates no sessions", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer frontDoor.Close()

	dir := t.TempDir()
	store := NewTaskStore(dir)
	logStore := NewLogStore(filepath.Join(dir, "task-logs"))
	hub := NewHub(store)
	s := NewScheduler(NewRelayClient(frontDoor.URL, "", ""), store, logStore, hub)

	wsServer := httptest.NewServer(HandleWS(hub))
	defer wsServer.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial task WS: %v", err)
	}
	defer conn.Close()
	var frame map[string]interface{}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&frame); err != nil || frame["type"] != "task_status" {
		t.Fatalf("first WS frame = %v (%v), want task_status", frame, err)
	}

	task, err := store.Create(Task{
		Name:      "nightly digest",
		ProjectID: "p1",
		Prompt:    "summarize",
		Enabled:   true,
		Schedule:  json.RawMessage(`{"type":"interval","minutes":15}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	s.executeTask(*task)

	if n := sessionPosts.Load(); n != 0 {
		t.Errorf("front door got %d POST /api/sessions, want 0", n)
	}
	if got, err := store.Get(task.ID); err != nil || got.LastStatus != "error" {
		t.Errorf("stored task = %+v (%v), want lastStatus error", got, err)
	}
	history := logStore.Load("p1", task.ID)
	if len(history) != 1 || history[0].Status != "error" ||
		!strings.Contains(history[0].Error, task.Name) || !strings.Contains(history[0].Error, "model") {
		t.Errorf("history = %+v, want one error run whose error names the task and model", history)
	}
	for frame["type"] != "task_error" {
		frame = nil
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("no task_error broadcast: %v", err)
		}
	}
	if frame["taskName"] != task.Name {
		t.Errorf("task_error taskName = %v, want %q", frame["taskName"], task.Name)
	}
}
