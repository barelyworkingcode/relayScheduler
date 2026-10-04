package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestNewTraceID_Is32LowercaseHex(t *testing.T) {
	a, b := newTraceID(), newTraceID()
	if !hex32.MatchString(a) || !hex32.MatchString(b) {
		t.Fatalf("bad ids %q %q", a, b)
	}
	if a == b {
		t.Error("two ids equal")
	}
	if !validTraceID(a) {
		t.Error("generated id is not valid")
	}
}

func TestValidTraceID(t *testing.T) {
	cases := map[string]bool{
		"":                          false,
		"abcdefg":                   false,
		"abcdefgh":                  true,
		strings.Repeat("a", 64):     true,
		strings.Repeat("a", 65):     false,
		"abc def12":                 false,
		"abcdefgh\n":                false,
		"abc\n{\"msg\":\"forged\"}": false,
		"abcdéfgh":                  false,
		"ab-cd_EF12":                true,
	}
	for in, want := range cases {
		if got := validTraceID(in); got != want {
			t.Errorf("validTraceID(%q) = %v, want %v", in, got, want)
		}
	}
}

// newLoggedAPI serves the real task API behind traceMiddleware and captures
// the default logger's output.
func newLoggedAPI(t *testing.T) (http.Handler, *bytes.Buffer) {
	h, buf, _ := newLoggedAPIDir(t)
	return h, buf
}

func newLoggedAPIDir(t *testing.T) (http.Handler, *bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(newLogger(buf, "info", fixedClock()))
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := t.TempDir()
	store := NewTaskStore(dir)
	logStore := NewLogStore(filepath.Join(dir, "task-logs"))
	mux := http.NewServeMux()
	RegisterRoutes(mux, store, NewScheduler(nil, store, logStore, NewHub(store)), logStore)
	return traceMiddleware(mux), buf, dir
}

func createLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, m := range parseLines(t, buf.String()) {
		if m["op"] == "schedule.create" {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d schedule.create lines, want 1: %s", len(found), buf.String())
	}
	return found[0]
}

func TestAPI_CreateLogsScheduleCreate(t *testing.T) {
	h, buf := newLoggedAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(chatTaskJSON))
	req.Header.Set("X-Trace-Id", "trace-abc12345")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	id := decodeTaskID(t, rec.Body.Bytes())
	m := createLine(t, buf)
	if m["level"] != "info" || m["status"] != "ok" || m["error"] != "" {
		t.Errorf("bad line: %v", m)
	}
	if d, _ := m["duration_ms"].(float64); d < 0 {
		t.Errorf("duration_ms %v", m["duration_ms"])
	}
	if m["job_id"] != id {
		t.Errorf("job_id = %v, want %s", m["job_id"], id)
	}
	if m["trace_id"] != "trace-abc12345" {
		t.Errorf("trace_id = %v", m["trace_id"])
	}
}

func TestAPI_FailedCreateLogsWarnOrError(t *testing.T) {
	const canary = "CANARY-PROMPT-5d2e"
	t.Run("invalid body is denied", func(t *testing.T) {
		h, buf := newLoggedAPI(t)
		req := httptest.NewRequest(http.MethodPost, "/api/tasks",
			strings.NewReader(`{"prompt":"`+canary+`","schedule":{"type":"nope"}}`))
		req.Header.Set("X-Trace-Id", "trace-bad12345")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
		}
		m := createLine(t, buf)
		if m["level"] != "warn" || m["status"] != "denied" || m["trace_id"] != "trace-bad12345" {
			t.Errorf("bad line: %v", m)
		}
		if d, ok := m["duration_ms"].(float64); !ok || d < 0 {
			t.Errorf("duration_ms %v", m["duration_ms"])
		}
		if strings.Contains(buf.String(), canary) {
			t.Errorf("body leaked into log: %s", buf.String())
		}
	})
	t.Run("store failure is an error", func(t *testing.T) {
		h, buf, dir := newLoggedAPIDir(t)
		// A directory where tasks.json belongs makes every store call fail,
		// for root too.
		if err := os.Mkdir(filepath.Join(dir, "tasks.json"), 0700); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(chatTaskJSON, "summarize", canary, 1)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body)))
		if rec.Code < 500 {
			t.Fatalf("status %d, want 5xx: %s", rec.Code, rec.Body)
		}
		m := createLine(t, buf)
		if m["level"] != "error" || m["status"] != "error" || m["error"] == "" {
			t.Errorf("bad line: %v", m)
		}
		if strings.Contains(buf.String(), canary) {
			t.Errorf("body leaked into log: %s", buf.String())
		}
	})
}

func decodeTaskID(t *testing.T, body []byte) string {
	t.Helper()
	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func TestTraceMiddleware_InboundIDHandling(t *testing.T) {
	forged := "abc\n{\"msg\":\"forged\"}"
	cases := []struct {
		name    string
		values  []string
		want    string // "" means a fresh 32-hex id
		rejects string // value that must not appear in the log
	}{
		{"missing", nil, "", ""},
		{"empty", []string{""}, "", ""},
		{"7 chars", []string{"abcdefg"}, "", "abcdefg"},
		{"65 chars", []string{strings.Repeat("a", 65)}, "", strings.Repeat("a", 65)},
		{"spaces", []string{"abc def ghi"}, "", "abc def ghi"},
		{"newline injection", []string{forged}, "", "forged"},
		{"unicode", []string{"abcdéfgh12"}, "", "abcdéfgh12"},
		{"8 chars", []string{"abcdefgh"}, "abcdefgh", ""},
		{"64 chars", []string{strings.Repeat("b", 64)}, strings.Repeat("b", 64), ""},
		{"two values first wins", []string{"firstid1", "secondid2"}, "firstid1", "secondid2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, buf := newLoggedAPI(t)
			req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(chatTaskJSON))
			req.Header["X-Trace-Id"] = c.values
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d", rec.Code)
			}
			got := createLine(t, buf)["trace_id"].(string)
			if c.want != "" && got != c.want {
				t.Errorf("trace_id = %q, want %q", got, c.want)
			}
			if c.want == "" && !hex32.MatchString(got) {
				t.Errorf("trace_id = %q, want fresh 32 hex", got)
			}
			if c.rejects != "" && strings.Contains(buf.String(), c.rejects) {
				t.Errorf("rejected value leaked into log: %s", buf.String())
			}
		})
	}
}

func TestTraceMiddleware_ContextCarriesID(t *testing.T) {
	var seen string
	h := traceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = traceFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Trace-Id", "ctxid-1234")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "ctxid-1234" {
		t.Errorf("traceFrom = %q", seen)
	}
}

func TestAPI_CreateNeverLogsPromptOrToken(t *testing.T) {
	h, buf := newLoggedAPI(t)
	body := `{"name":"digest","projectId":"p1","prompt":"CANARY-PROMPT-9f3a","model":"haiku","enabled":true,` +
		`"token":"CANARY-TOKEN-77c1","schedule":{"type":"interval","minutes":15}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body)))
	// A bad body too: error paths must not echo it.
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"prompt":"CANARY-PROMPT-9f3a","token":"CANARY-TOKEN-77c1","schedule":{"type":"nope"}}`)))
	if bad.Code == http.StatusCreated {
		t.Fatal("bad body accepted")
	}
	if strings.Contains(buf.String(), "CANARY") {
		t.Errorf("canary leaked into logs: %s", buf.String())
	}
}
