package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const outputJSON = `{"renderer":"list","items":[]}`

// ptyRelay fakes relay's front door for project p1, whose path is dir. Each
// terminal attach plays the task's script: it runs action against dir, then
// sends terminal_exit with exitCode. A negative exitCode never exits, so the
// run times out.
type ptyRelay struct {
	t        *testing.T
	dir      string
	mu       sync.Mutex
	action   func(dir string) error
	exitCode int
}

func (f *ptyRelay) next(action func(dir string) error, exitCode int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.action, f.exitCode = action, exitCode
}

func (f *ptyRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/projects/p1":
		json.NewEncoder(w).Encode(map[string]string{"id": "p1", "path": f.dir})
	case r.Method == http.MethodPost && r.URL.Path == "/api/terminals":
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"terminalId":"term1"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/terminals/term1/log":
		w.Write([]byte("noise\r\n"))
	case r.URL.Path == "/ws":
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil { // join_terminal
			return
		}
		f.mu.Lock()
		action, code := f.action, f.exitCode
		f.mu.Unlock()
		if action != nil {
			if err := action(f.dir); err != nil {
				f.t.Errorf("script action: %v", err)
			}
		}
		if code < 0 {
			conn.ReadMessage() // hold until the scheduler gives up
			return
		}
		conn.WriteJSON(map[string]interface{}{"type": "terminal_exit", "exitCode": code})
	default:
		w.WriteHeader(http.StatusOK)
	}
}

// newPtyRun creates the PTY task (ptyTaskJSON plus fields) through the API,
// wired to a scheduler whose relay is a ptyRelay.
func newPtyRun(t *testing.T, fields string) (*ptyRelay, http.Handler, *Scheduler, string) {
	t.Helper()
	relay := &ptyRelay{t: t, dir: t.TempDir()}
	srv := httptest.NewServer(relay)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	store := NewTaskStore(dir)
	logStore := NewLogStore(filepath.Join(dir, "task-logs"))
	s := NewScheduler(NewRelayClient(srv.URL, "", ""), store, logStore, NewHub(store))
	mux := http.NewServeMux()
	RegisterRoutes(mux, store, s, logStore)
	return relay, mux, s, createTask(t, mux, withFields(ptyTaskJSON, fields)).ID
}

// runAndWait runs the task with RunTaskNow and returns the history route's
// records, newest first, once run number `runs` is recorded and settled.
func runAndWait(t *testing.T, h http.Handler, s *Scheduler, id string, runs int) []map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for err := s.RunTaskNow(id); err != nil; err = s.RunTaskNow(id) {
		if !errors.Is(err, ErrTaskRunning) || time.Now().After(deadline) {
			t.Fatalf("RunTaskNow: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		var history []map[string]interface{}
		_ = json.Unmarshal(serve(h, http.MethodGet, "/api/tasks/"+id+"/history", "").Body.Bytes(), &history)
		var task struct {
			LastStatus string `json:"lastStatus"`
		}
		_ = json.Unmarshal(serve(h, http.MethodGet, "/api/tasks/"+id, "").Body.Bytes(), &task)
		if len(history) == runs && task.LastStatus != "running" {
			return history
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d not recorded in 5s: history %v, lastStatus %q", runs, history, task.LastStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeOutput(content string) func(dir string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "today.json"), []byte(content), 0600)
	}
}

func TestRunPty_OutputFile(t *testing.T) {
	const withOutput = `,"outputFile":"today.json"`
	atCap := strings.Repeat("x", 64*1024)
	outside := t.TempDir()

	for _, tc := range []struct {
		name       string
		fields     string
		setup      func(dir string) error // before the run starts
		action     func(dir string) error // during the run
		exitCode   int                    // negative: never exits
		wantStatus string
		wantError  string
		wantOutput string // empty: no output key
	}{
		{name: "success", fields: withOutput, action: writeOutput(outputJSON),
			wantStatus: "success", wantOutput: outputJSON},
		{name: "exactly at the cap", fields: withOutput, action: writeOutput(atCap),
			wantStatus: "success", wantOutput: atCap},
		{name: "missing", fields: withOutput,
			wantStatus: "error", wantError: "output file not produced"},
		{name: "stale", fields: withOutput,
			setup: func(dir string) error {
				if err := writeOutput(outputJSON)(dir); err != nil {
					return err
				}
				old := time.Now().Add(-time.Hour)
				return os.Chtimes(filepath.Join(dir, "today.json"), old, old)
			},
			wantStatus: "error", wantError: "output file not produced"},
		{name: "over the cap", fields: withOutput, action: writeOutput(atCap + "x"),
			wantStatus: "error", wantError: "output file is over the 64 KB cap"},
		{name: "symlink to a file outside the project", fields: withOutput,
			action: func(dir string) error {
				target := filepath.Join(outside, "elsewhere.json")
				if err := os.WriteFile(target, []byte(outputJSON), 0600); err != nil {
					return err
				}
				return os.Symlink(target, filepath.Join(dir, "today.json"))
			},
			wantStatus: "error", wantError: "output file is a symlink"},
		{name: "FIFO", fields: withOutput,
			action:     func(dir string) error { return syscall.Mkfifo(filepath.Join(dir, "today.json"), 0600) },
			wantStatus: "error", wantError: "output file is not a regular file"},
		{name: "timeout with a fresh file", fields: withOutput + `,"maxDurationSeconds":1`,
			action: writeOutput(outputJSON), exitCode: -1,
			wantStatus: "timeout", wantError: "task exceeded maxDurationSeconds"},
		{name: "no outputFile", action: writeOutput(outputJSON),
			wantStatus: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			relay, h, s, id := newPtyRun(t, tc.fields)
			if tc.setup != nil {
				if err := tc.setup(relay.dir); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			relay.next(tc.action, tc.exitCode)

			rec := runAndWait(t, h, s, id, 1)[0]

			gotError, _ := rec["error"].(string)
			if rec["status"] != tc.wantStatus || gotError != tc.wantError {
				t.Errorf("status, error = %v, %q; want %s, %q", rec["status"], gotError, tc.wantStatus, tc.wantError)
			}
			wantExit := float64(tc.exitCode)
			if tc.exitCode < 0 {
				wantExit = ExitCodeTimeout
			}
			if rec["exitCode"] != wantExit {
				t.Errorf("exitCode = %v, want %v", rec["exitCode"], wantExit)
			}
			gotOutput, present := rec["output"]
			if tc.wantOutput == "" && present {
				t.Errorf("output present (%d bytes), want no output key", len(gotOutput.(string)))
			}
			if tc.wantOutput != "" && gotOutput != tc.wantOutput {
				t.Errorf("output = %.80q, want %.80q", gotOutput, tc.wantOutput)
			}
			if rec["response"] != "noise\r\n" {
				t.Errorf("response = %q, want the terminal tail %q", rec["response"], "noise\r\n")
			}
		})
	}
}

func TestRunPty_FailedRunKeepsEarlierOutput(t *testing.T) {
	relay, h, s, id := newPtyRun(t, `,"outputFile":"today.json"`)

	relay.next(writeOutput(outputJSON), 0)
	runAndWait(t, h, s, id, 1)

	relay.next(writeOutput(`{"renderer":"list","items":["fresh"]}`), 3)
	history := runAndWait(t, h, s, id, 2)

	failed, earlier := history[0], history[1]
	if _, present := failed["output"]; present || failed["status"] != "error" || failed["error"] != "process exited with code 3" {
		t.Errorf("exit-3 run = status %v, error %v, output %v; want error, \"process exited with code 3\", no output",
			failed["status"], failed["error"], failed["output"])
	}
	if earlier["output"] != outputJSON {
		t.Errorf("earlier run's output = %v, want %s kept in history", earlier["output"], outputJSON)
	}
}
