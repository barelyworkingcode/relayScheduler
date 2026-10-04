package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNonce = "abcd1234"

func decodeEntry(t *testing.T, raw string) historyEntry {
	t.Helper()
	var es []historyEntry
	if err := json.Unmarshal([]byte(raw), &es); err != nil || len(es) != 1 {
		t.Fatalf("bad fixture: %v", err)
	}
	return es[0]
}

func TestClassifyShellRun(t *testing.T) {
	marker := markerFor(testNonce)
	// Shaped as relayScheduler's Execution serialises a finished PTY run.
	passJSON := `[{"taskId":"t1","taskName":"n","projectId":"p1","startedAt":"2026-01-01T00:00:00Z",` +
		`"completedAt":"2026-01-01T00:00:01Z","status":"success","response":"relayscheduler-verify-abcd1234\r\n",` +
		`"terminalId":"term1","exitCode":0}]`
	nilExit := `[{"status":"error","error":"boom"}]`
	tmplErr := `[{"status":"error","error":"create terminal: template \"world-probe\" not allowed"}]`
	nonzero := `[{"status":"error","exitCode":3,"error":"exited 3\nmore"}]`
	notOK := `[{"status":"timeout","exitCode":0,"response":"relayscheduler-verify-abcd1234"}]`
	noMarker := `[{"status":"success","exitCode":0,"response":"something else\n"}]`
	cases := []struct {
		name   string
		ended  bool
		json   string
		want   state
		detail string
	}{
		{"not ended", false, `[{}]`, stateFail, "did not end within 60s"},
		{"template error", true, tmplErr, stateBlocked, "world-probe"},
		{"nil exit code", true, nilExit, stateFail, "no exit code"},
		{"nonzero exit", true, nonzero, stateFail, "exit 3: exited 3"},
		{"status not success", true, notOK, stateFail, "timeout"},
		{"marker missing", true, noMarker, stateFail, "lacks the marker"},
		{"marker present", true, passJSON, statePass, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := classifyShellRun(c.ended, decodeEntry(t, c.json), marker)
			if r.State != c.want || !strings.Contains(r.Detail, c.detail) {
				t.Fatalf("got %s %q, want %s containing %q", r.State, r.Detail, c.want, c.detail)
			}
		})
	}
}

// fakeDoor is a stand-in for relay's front door on a unix socket.
type fakeDoor struct {
	mu                       sync.Mutex
	createStatus, runStatus  int
	delStatus                int
	history                  string
	getAfterDeleteStaysFound bool
	createBody               map[string]any
	auth                     string
	runCalls                 int
	termDeleted, deleted     bool
}

func startDoor(t *testing.T, f *fakeDoor) env {
	t.Helper()
	dir, err := os.MkdirTemp("", "dv")
	if err != nil || dir == "" {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if f.createStatus == 0 {
		f.createStatus = 201
	}
	if f.runStatus == 0 {
		f.runStatus = 200
	}
	if f.delStatus == 0 {
		f.delStatus = 200
	}
	if f.history == "" {
		f.history = "[]"
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, body string) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&f.createBody)
		reply(w, f.createStatus, `{"id":"t1","error":"nope"}`)
	})
	mux.HandleFunc("POST /api/tasks/t1/run", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.runCalls++
		reply(w, f.runStatus, `{"error":"run refused"}`)
	})
	mux.HandleFunc("GET /api/tasks/t1/history", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply(w, 200, f.history)
	})
	mux.HandleFunc("GET /api/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.deleted && !f.getAfterDeleteStaysFound {
			reply(w, 404, `{"error":"not found"}`)
			return
		}
		reply(w, 200, `{"id":"t1","lastTerminalId":"term1"}`)
	})
	mux.HandleFunc("DELETE /api/tasks/t1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.delStatus == 200 {
			f.deleted = true
		}
		reply(w, f.delStatus, `{"deleted":true}`)
	})
	mux.HandleFunc("DELETE /api/terminals/term1", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.termDeleted = true
		reply(w, 200, `{}`)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return env{FrontendSocket: sock, Credential: "tok-123", ProjectID: "p1", ProjectName: "Acme", Nonce: testNonce}
}

func fastPolling(t *testing.T) {
	t.Helper()
	d, p := runDeadline, pollEvery
	runDeadline, pollEvery = 400*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { runDeadline, pollEvery = d, p })
}

const okHistory = `[{"status":"success","exitCode":0,"response":"relayscheduler-verify-abcd1234\n"}]`

func TestRunShellRoutinePass(t *testing.T) {
	fastPolling(t)
	f := &fakeDoor{history: okHistory}
	e := startDoor(t, f)
	r := runShellRoutine(context.Background(), e)
	if r.State != statePass {
		t.Fatalf("got %s %q", r.State, r.Detail)
	}
	if f.auth != "Bearer tok-123" {
		t.Errorf("auth header = %q", f.auth)
	}
	b := f.createBody
	if b["sessionType"] != "pty" || b["templateId"] != "world-probe" || b["projectId"] != "p1" {
		t.Errorf("create body = %v", b)
	}
	if s, _ := b["schedule"].(map[string]any); s["type"] != "on_demand" {
		t.Errorf("schedule = %v", b["schedule"])
	}
	args, _ := json.Marshal(b["extraArgs"])
	if strings.Contains(string(args), "relayscheduler-verify-"+testNonce) {
		t.Errorf("finished marker appears in extraArgs: %s", args)
	}
	if f.runCalls != 1 || !f.deleted || !f.termDeleted {
		t.Errorf("run=%d deleted=%v terminalDeleted=%v", f.runCalls, f.deleted, f.termDeleted)
	}
}

func TestRunShellRoutineCleansUpOnEveryOutcome(t *testing.T) {
	fastPolling(t)
	cases := []struct {
		name      string
		runStatus int
		detail    string
	}{
		{"timeout", 0, "did not end"},
		{"run error", 500, "run: status 500"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeDoor{runStatus: c.runStatus}
			r := runShellRoutine(context.Background(), startDoor(t, f))
			if r.State != stateFail || !strings.Contains(r.Detail, c.detail) {
				t.Fatalf("got %s %q", r.State, r.Detail)
			}
			if !f.deleted {
				t.Fatal("routine not deleted")
			}
		})
	}
}

func TestRunShellRoutineLeftRoutineFailsPass(t *testing.T) {
	fastPolling(t)
	cases := []struct {
		name string
		door *fakeDoor
	}{
		{"delete fails", &fakeDoor{history: okHistory, delStatus: 500}},
		{"still there after delete", &fakeDoor{history: okHistory, getAfterDeleteStaysFound: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runShellRoutine(context.Background(), startDoor(t, c.door))
			if r.State != stateFail || !strings.Contains(r.Detail, "left routine verify-"+testNonce+"-shell") {
				t.Fatalf("got %s %q", r.State, r.Detail)
			}
		})
	}
}

func TestRunShellRoutineCredentialRefused(t *testing.T) {
	fastPolling(t)
	for status, want := range map[int]string{401: "401", 403: "403"} {
		f := &fakeDoor{createStatus: status}
		r := runShellRoutine(context.Background(), startDoor(t, f))
		if r.State != stateBlocked || !strings.Contains(r.Detail, want) {
			t.Errorf("status %d: got %s %q", status, r.State, r.Detail)
		}
		if f.runCalls != 0 {
			t.Errorf("status %d: run was called", status)
		}
	}
}
