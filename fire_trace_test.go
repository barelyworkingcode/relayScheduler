package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	fireCanaryBody   = "CANARY-BODY-5521"
	fireCanaryPrompt = "CANARY-PROMPT-8841"
	fireCanaryToken  = "CANARY-BEARER-3307"
)

// lockedBuf is a log sink safe for the goroutines a fire spawns.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuf) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(newLogger(buf, "debug", fixedClock()))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type relayCall struct{ method, path, trace string }

// fireRelay is a fake relay front door that records every request (including
// the WS upgrade) with its X-Trace-Id, and every WS frame the client sends.
type fireRelay struct {
	*httptest.Server
	mu           sync.Mutex
	calls        []relayCall
	frames       []map[string]any
	hang         bool // chat turn never completes
	projectFails bool
}

func newFireRelay(t *testing.T) *fireRelay {
	t.Helper()
	f := &fireRelay{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, relayCall{r.Method, r.URL.Path, r.Header.Get("X-Trace-Id")})
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/ws":
			f.serveWS(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/p1":
			if f.projectFails {
				http.Error(w, fireCanaryBody, http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"id":"p1","path":"/work"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"sessionId":"s-new1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/terminals":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"terminalId":"t-new1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/terminals/t-new1/log":
			w.Write([]byte("output"))
		default:
			w.WriteHeader(http.StatusOK) // deletes, stop, close
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fireRelay) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var m map[string]any
		if conn.ReadJSON(&m) != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, m)
		f.mu.Unlock()
		switch m["type"] {
		case "join_terminal":
			_ = conn.WriteJSON(map[string]any{"type": "terminal_exit", "exitCode": 0})
		case "send_message":
			if !f.hang {
				_ = conn.WriteJSON(map[string]any{"type": "message_complete", "sessionId": "s-new1"})
			}
		}
	}
}

func (f *fireRelay) snapshot() ([]relayCall, []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relayCall(nil), f.calls...), append([]map[string]any(nil), f.frames...)
}

// assertTraced requires every recorded call to carry want and every
// "METHOD path" in required to have been made.
func (f *fireRelay) assertTraced(t *testing.T, want string, required ...string) {
	t.Helper()
	calls, _ := f.snapshot()
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.method+" "+c.path] = true
		if c.trace != want {
			t.Errorf("%s %s carried X-Trace-Id %q, want %q", c.method, c.path, c.trace, want)
		}
	}
	for _, r := range required {
		if !seen[r] {
			t.Errorf("relay never saw %s; calls: %v", r, calls)
		}
	}
}

type fireEnv struct {
	s     *Scheduler
	store *TaskStore
	logs  *LogStore
	hub   *Hub
	relay *fireRelay
	buf   *lockedBuf
}

func newFireEnv(t *testing.T) *fireEnv {
	t.Helper()
	buf := captureLogs(t)
	relay := newFireRelay(t)
	dir := t.TempDir()
	store := NewTaskStore(dir)
	logs := NewLogStore(filepath.Join(dir, "task-logs"))
	hub := NewHub(store)
	return &fireEnv{
		s:     NewScheduler(NewRelayClient(relay.URL, "", fireCanaryToken), store, logs, hub),
		store: store, logs: logs, hub: hub, relay: relay, buf: buf,
	}
}

func (e *fireEnv) chatTask(t *testing.T, maxSeconds int) *Task {
	t.Helper()
	task, err := e.store.Create(Task{
		Name: "digest", ProjectID: "p1", Prompt: fireCanaryPrompt, Model: "haiku",
		Enabled: true, Schedule: json.RawMessage(`{"type":"on_demand"}`), MaxDurationSeconds: maxSeconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (e *fireEnv) ptyTask(t *testing.T) *Task {
	t.Helper()
	task, err := e.store.Create(Task{
		Name: "script", ProjectID: "p1", SessionType: SessionTypePTY, TemplateID: "shell",
		Enabled: true, Schedule: json.RawMessage(`{"type":"on_demand"}`), MaxDurationSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// linesByOp returns the schema-valid log lines with the given op.
func linesByOp(t *testing.T, buf *lockedBuf, op string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range parseLines(t, buf.String()) {
		if m["op"] == op {
			out = append(out, m)
		}
	}
	return out
}

// waitForOp waits for n lines of op; fires started by RunTaskNow or a tick
// run on their own goroutine.
func waitForOp(t *testing.T, buf *lockedBuf, op string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, raw := range strings.Split(buf.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil && m["op"] == op {
				count++
			}
		}
		if count >= n {
			return linesByOp(t, buf, op)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s lines; got: %s", n, op, buf.String())
	return nil
}

func assertNoSecrets(t *testing.T, buf *lockedBuf) {
	t.Helper()
	for _, s := range []string{fireCanaryBody, fireCanaryPrompt, fireCanaryToken} {
		if strings.Contains(buf.String(), s) {
			t.Errorf("log contains %q: %s", s, buf.String())
		}
	}
}

func TestFire_ChatRunTracesEveryCallAndTimeoutIsError(t *testing.T) {
	e := newFireEnv(t)
	e.relay.hang = true
	task := e.chatTask(t, 1)
	e.store.SetLastSessionID(task.ID, "old-sess")

	e.s.executeTask(*task, "chattrace-01", "schedule")

	e.relay.assertTraced(t, "chattrace-01",
		"GET /api/projects/p1", "POST /api/sessions/old-sess/delete", "POST /api/sessions",
		"GET /ws", "POST /api/sessions/s-new1/stop")
	_, frames := e.relay.snapshot()
	var sent map[string]any
	for _, f := range frames {
		if f["type"] == "send_message" {
			sent = f
		}
	}
	if sent == nil || sent["trace_id"] != "chattrace-01" {
		t.Errorf("send_message frame = %v, want trace_id chattrace-01", sent)
	}
	fires := linesByOp(t, e.buf, "job.fire")
	if len(fires) != 1 || fires[0]["status"] != "error" || fires[0]["error"] == "" {
		t.Errorf("job.fire lines for a timed-out run = %v, want one with status error", fires)
	}
	assertNoSecrets(t, e.buf)
}

func TestFire_PtyRunTracesEveryCall(t *testing.T) {
	e := newFireEnv(t)
	task := e.ptyTask(t)
	e.store.SetLastTerminalID(task.ID, "old-term")

	e.s.executeTask(*task, "ptytrace-01", "run_now")

	e.relay.assertTraced(t, "ptytrace-01",
		"GET /api/projects/p1", "DELETE /api/terminals/old-term", "POST /api/terminals",
		"GET /ws", "GET /api/terminals/t-new1/log")
	fires := linesByOp(t, e.buf, "job.fire")
	if len(fires) != 1 || fires[0]["status"] != "ok" || fires[0]["trigger"] != "run_now" {
		t.Errorf("job.fire lines = %v, want one ok run_now", fires)
	}
}

func TestFire_TwoFiresShareJobIDAndHaveDistinctRunIDs(t *testing.T) {
	e := newFireEnv(t)
	task := e.chatTask(t, 5)

	srv := httptest.NewServer(HandleWS(e.hub))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	e.s.executeTask(*task, "t1xxxxxxxx", "schedule")
	e.s.executeTask(*task, "t1xxxxxxxx", "schedule")

	fires := linesByOp(t, e.buf, "job.fire")
	if len(fires) != 2 {
		t.Fatalf("got %d job.fire lines, want 2: %s", len(fires), e.buf.String())
	}
	for _, m := range fires {
		if m["status"] != "ok" || m["level"] != "info" || m["trigger"] != "schedule" ||
			m["error"] != "" || m["trace_id"] != "t1xxxxxxxx" || m["job_id"] != task.ID {
			t.Errorf("bad job.fire line: %v", m)
		}
		if d, ok := m["duration_ms"].(float64); !ok || d < 0 {
			t.Errorf("duration_ms = %v", m["duration_ms"])
		}
		if id, _ := m["run_id"].(string); !hex32.MatchString(id) {
			t.Errorf("run_id = %v, want 32 hex", m["run_id"])
		}
	}
	if fires[0]["run_id"] == fires[1]["run_id"] {
		t.Errorf("both fires share run_id %v", fires[0]["run_id"])
	}

	// The wire runId stays the session id and is not the log run_id.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("no task_completed broadcast: %v", err)
		}
		if frame["type"] != "task_completed" {
			continue
		}
		view, _ := frame["view"].(map[string]any)
		if view["runId"] != "s-new1" || view["runId"] == fires[0]["run_id"] {
			t.Errorf("task_completed view = %v, want runId s-new1 distinct from log run_id", view)
		}
		break
	}
	if h := e.logs.Load("p1", task.ID); len(h) == 0 || h[0].SessionID != "s-new1" {
		t.Errorf("history = %+v, want SessionID s-new1", h)
	}
	assertNoSecrets(t, e.buf)
}

func TestFire_FailedRunIsErrorWithoutLeakingBody(t *testing.T) {
	e := newFireEnv(t)
	e.relay.projectFails = true
	task := e.chatTask(t, 5)

	e.s.executeTask(*task, "failtrace-01", "run_now")

	fires := linesByOp(t, e.buf, "job.fire")
	if len(fires) != 1 {
		t.Fatalf("got %d job.fire lines, want 1: %s", len(fires), e.buf.String())
	}
	m := fires[0]
	if m["status"] != "error" || m["level"] != "error" || m["error"] == "" || m["trigger"] != "run_now" {
		t.Errorf("bad failure line: %v", m)
	}
	if d, ok := m["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms = %v", m["duration_ms"])
	}
	assertNoSecrets(t, e.buf)
}

func TestFire_RunNowCarriesInboundTrace(t *testing.T) {
	longID := strings.Repeat("b", 64)
	cases := []struct {
		name   string
		header []string // nil: no header
		direct string
		want   string // "" means a fresh 32-hex id
	}{
		{name: "direct call", direct: "inboundtrace1", want: "inboundtrace1"},
		{name: "valid header", header: []string{"valid-id-12345"}, want: "valid-id-12345"},
		{name: "64 char header", header: []string{longID}, want: longID},
		{name: "no header"},
		{name: "invalid header replaced", header: []string{"bad id!"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFireEnv(t)
			task := e.chatTask(t, 5)
			if c.direct != "" {
				if err := e.s.RunTaskNow(task.ID, c.direct); err != nil {
					t.Fatal(err)
				}
			} else {
				mux := http.NewServeMux()
				RegisterRoutes(mux, e.store, e.s, e.logs)
				req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID+"/run", nil)
				req.Header["X-Trace-Id"] = c.header
				rec := httptest.NewRecorder()
				traceMiddleware(mux).ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
			// The request has returned; the run must still finish and log.
			fire := waitForOp(t, e.buf, "job.fire", 1)[0]
			if fire["status"] != "ok" || fire["trigger"] != "run_now" {
				t.Errorf("bad job.fire line: %v", fire)
			}
			got, _ := fire["trace_id"].(string)
			if c.want == "" {
				if !hex32.MatchString(got) {
					t.Errorf("trace_id = %q, want fresh 32 hex", got)
				}
			} else if got != c.want {
				t.Errorf("trace_id = %q, want %q", got, c.want)
			}
			e.relay.assertTraced(t, got, "GET /api/projects/p1", "POST /api/sessions", "GET /ws")
			if strings.Contains(e.buf.String(), "bad id!") {
				t.Errorf("invalid inbound id leaked into log: %s", e.buf.String())
			}
		})
	}
}

func TestFire_TickGivesEachFireItsOwnTrace(t *testing.T) {
	e := newFireEnv(t)
	for _, name := range []string{"a", "b"} {
		// A blank model fails fast without a session; the fire still logs.
		task, err := e.store.Create(Task{
			Name: name, ProjectID: "p1", Prompt: "x", Enabled: true,
			Schedule: json.RawMessage(`{"type":"interval","minutes":15}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		e.s.ScheduleTask(*task)
		e.s.tasks[task.ID].nextRun = time.Now().Add(-time.Minute)
	}

	e.s.checkAndFireTasks()

	fires := waitForOp(t, e.buf, "job.fire", 2)
	a, b := fires[0]["trace_id"].(string), fires[1]["trace_id"].(string)
	if !hex32.MatchString(a) || !hex32.MatchString(b) || a == b {
		t.Errorf("tick trace ids %q %q, want two distinct fresh 32-hex ids", a, b)
	}
	for _, m := range fires {
		if m["trigger"] != "schedule" {
			t.Errorf("trigger = %v, want schedule", m["trigger"])
		}
	}
}

func TestTick_NothingDueWritesNoLines(t *testing.T) {
	e := newFireEnv(t)
	task := e.chatTask(t, 5)
	e.s.ScheduleTask(*task) // on_demand: never due
	e.buf.Reset()

	e.s.checkAndFireTasks()

	if out := e.buf.String(); out != "" {
		t.Errorf("idle tick wrote log lines: %s", out)
	}
}

func TestTick_SkippedMissedTaskWritesOneJobSkip(t *testing.T) {
	cases := []struct{ name, schedule string }{
		{"recurring", `{"type":"interval","minutes":15}`},
		{"once", `{"type":"once","at":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFireEnv(t)
			task, err := e.store.Create(Task{
				Name: "late", ProjectID: "p1", Prompt: "x", Model: "haiku", Enabled: true,
				Schedule: json.RawMessage(c.schedule),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.s.LoadAllTasks(); err != nil {
				t.Fatal(err)
			}
			e.s.tasks[task.ID].nextRun = time.Now().Add(-time.Hour)

			e.s.checkAndFireTasks()

			skips := linesByOp(t, e.buf, "job.skip")
			if len(skips) != 1 {
				t.Fatalf("got %d job.skip lines, want 1: %s", len(skips), e.buf.String())
			}
			m := skips[0]
			if m["job_id"] != task.ID || m["status"] != "ok" || m["duration_ms"] != float64(0) {
				t.Errorf("bad job.skip line: %v", m)
			}
			if fires := linesByOp(t, e.buf, "job.fire"); len(fires) != 0 {
				t.Errorf("skipped task logged a fire: %v", fires)
			}
		})
	}
}

func TestClient_TraceHeaderStaysOnBox(t *testing.T) {
	cases := []struct {
		baseURL, socket string
		want            bool
	}{
		{"http://localhost:3000", "", true},
		{"http://127.0.0.1:3000", "", true},
		{"http://[::1]:3000", "", true},
		{"http://relay.example.com", "/tmp/relay-front.sock", true},
		{"http://relay.example.com", "", false},
		{"http://192.168.1.5:3000", "", false},
		{"http://10.0.0.1", "", false},
	}
	for _, tc := range cases {
		client := NewRelayClient(tc.baseURL, tc.socket, "").withTrace("testtrace1")
		req, err := client.newRequest(http.MethodGet, "/api/projects/p1", nil)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if tc.want {
			want = "testtrace1"
		}
		if got := req.Header.Get("X-Trace-Id"); got != want {
			t.Errorf("%s socket=%q: X-Trace-Id = %q, want %q", tc.baseURL, tc.socket, got, want)
		}
	}
}
