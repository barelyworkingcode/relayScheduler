package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var nineKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

// checkSchema is a minimal JSON Schema checker. It supports exactly the
// keywords the logging schema uses and fails on any other, so a schema change
// cannot pass unchecked.
func checkSchema(schema map[string]any, v any, path string) []string {
	var errs []string
	for k, sv := range schema {
		switch k {
		case "$schema", "$id", "title", "description":
		case "type":
			if !typeMatches(sv.(string), v) {
				errs = append(errs, fmt.Sprintf("%s: want type %v, got %T", path, sv, v))
			}
		case "required":
			obj, _ := v.(map[string]any)
			for _, r := range sv.([]any) {
				if _, ok := obj[r.(string)]; !ok {
					errs = append(errs, fmt.Sprintf("%s: missing required %q", path, r))
				}
			}
		case "properties":
			obj, _ := v.(map[string]any)
			for name, sub := range sv.(map[string]any) {
				if val, ok := obj[name]; ok {
					errs = append(errs, checkSchema(sub.(map[string]any), val, path+"."+name)...)
				}
			}
		case "enum":
			found := false
			for _, e := range sv.([]any) {
				if e == v {
					found = true
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("%s: %v not in enum %v", path, v, sv))
			}
		case "pattern":
			if s, ok := v.(string); ok && !regexp.MustCompile(sv.(string)).MatchString(s) {
				errs = append(errs, fmt.Sprintf("%s: %q does not match %s", path, s, sv))
			}
		case "maxLength":
			if s, ok := v.(string); ok && float64(utf8.RuneCountInString(s)) > sv.(float64) {
				errs = append(errs, fmt.Sprintf("%s: longer than %v", path, sv))
			}
		case "minLength":
			if s, ok := v.(string); ok && float64(utf8.RuneCountInString(s)) < sv.(float64) {
				errs = append(errs, fmt.Sprintf("%s: shorter than %v", path, sv))
			}
		case "minimum":
			if n, ok := v.(float64); ok && n < sv.(float64) {
				errs = append(errs, fmt.Sprintf("%s: %v below %v", path, n, sv))
			}
		case "additionalProperties":
			if sv != true {
				errs = append(errs, path+": additionalProperties other than true is not supported by this checker")
			}
		default:
			errs = append(errs, fmt.Sprintf("%s: unsupported schema keyword %q", path, k))
		}
	}
	return errs
}

func typeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	}
	return false
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/logging-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// parseLines asserts every line is one JSON object that satisfies the schema.
func parseLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	schema := loadSchema(t)
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("line is not a JSON object: %q: %v", raw, err)
		}
		for _, e := range checkSchema(schema, m, "$") {
			t.Errorf("schema violation in %s: %s", raw, e)
		}
		lines = append(lines, m)
	}
	return lines
}

func fixedClock() func() time.Time {
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t0 }
}

func TestSchemaCheckerRejectsUnknownKeyword(t *testing.T) {
	errs := checkSchema(map[string]any{"type": "string", "format": "uri"}, "x", "$")
	if len(errs) == 0 {
		t.Fatal("checker accepted an unsupported keyword")
	}
}

func TestLogger_EveryLevelIsSchemaValidJSONLines(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "debug", fixedClock())
	ctx := context.Background()
	l.DebugContext(ctx, "d")
	l.InfoContext(ctx, "i", "op", "job.fire", "status", "ok", "duration_ms", 5)
	l.WarnContext(ctx, "w", "op", "job.fire", "status", "denied")
	l.ErrorContext(ctx, "e", "op", "job.fire", "status", "error", "error", "boom", "job_id", "j1", "run_id", "r1")
	lines := parseLines(t, buf.String())
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %s", len(lines), buf.String())
	}
	for _, m := range lines {
		for _, k := range nineKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("line missing key %q: %v", k, m)
			}
		}
		if m["service"] == "" {
			t.Error("empty service")
		}
	}
	if lines[3]["job_id"] != "j1" || lines[3]["run_id"] != "r1" || lines[3]["error"] != "boom" {
		t.Errorf("caller attrs lost: %v", lines[3])
	}
}

func TestLogger_ServiceID(t *testing.T) {
	t.Setenv("RELAY_SERVICE_ID", "")
	var buf bytes.Buffer
	newLogger(&buf, "info", fixedClock()).Info("x")
	if got := parseLines(t, buf.String())[0]["service"]; got != defaultServiceID {
		t.Errorf("service = %v, want %s", got, defaultServiceID)
	}
	t.Setenv("RELAY_SERVICE_ID", "sched-acme")
	buf.Reset()
	newLogger(&buf, "info", fixedClock()).Info("x")
	if got := parseLines(t, buf.String())[0]["service"]; got != "sched-acme" {
		t.Errorf("service = %v, want sched-acme", got)
	}
}

func TestLogger_DebugOffByDefault(t *testing.T) {
	for _, level := range []string{"", "info", "warn", "bogus"} {
		var buf bytes.Buffer
		l := newLogger(&buf, level, fixedClock())
		l.Debug("hidden")
		if strings.Contains(buf.String(), "hidden") {
			t.Errorf("level %q emitted debug line: %s", level, buf.String())
		}
	}
	var buf bytes.Buffer
	newLogger(&buf, "warn", fixedClock()).Info("hidden-info")
	if buf.Len() != 0 {
		t.Errorf("warn level emitted info: %s", buf.String())
	}
}

func TestLogger_DebugWindowExpires(t *testing.T) {
	start := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	now := start
	var buf bytes.Buffer
	l := newLogger(&buf, "debug", func() time.Time { return now })
	l.Debug("debug-at-start")
	now = start.Add(29*time.Minute + 59*time.Second)
	l.Debug("debug-at-2959")
	msgs := map[string]bool{}
	for _, m := range parseLines(t, buf.String()) {
		msgs[m["msg"].(string)] = true
		if m["level"] != "debug" {
			t.Errorf("unexpected line in window: %v", m)
		}
	}
	if !msgs["debug-at-start"] || !msgs["debug-at-2959"] {
		t.Fatalf("debug lines inside window missing: %s", buf.String())
	}
	buf.Reset()
	now = start.Add(30 * time.Minute)
	l.Debug("expired-debug-1")
	l.Debug("expired-debug-2")
	l.Info("still-info")
	var warns, infos []map[string]any
	for _, m := range parseLines(t, buf.String()) {
		switch m["level"] {
		case "warn":
			warns = append(warns, m)
		case "info":
			infos = append(infos, m)
		default:
			t.Errorf("unexpected line after window: %v", m)
		}
	}
	if len(warns) != 1 || len(infos) != 1 || infos[0]["msg"] != "still-info" {
		t.Fatalf("warns=%d infos=%d, want 1 and 1: %s", len(warns), len(infos), buf.String())
	}
	w := warns[0]
	if w["op"] != "log.level" || w["status"] != "error" || w["error"] == "" {
		t.Errorf("bad expiry warning: %v", w)
	}
	if strings.Contains(buf.String(), "expired-debug") {
		t.Error("expired debug message leaked")
	}
}

func TestInitLogging_ReadsLevelFromEnvAndWritesJSONToStderr(t *testing.T) {
	prevLogger, prevStderr := slog.Default(), os.Stderr
	t.Cleanup(func() { slog.SetDefault(prevLogger); os.Stderr = prevStderr })
	run := func(level string) string {
		t.Setenv("RELAY_LOG_LEVEL", level)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		initLogging()
		slog.Debug("dbg-line")
		slog.Info("info-line")
		w.Close()
		os.Stderr = prevStderr
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out)
	}
	def := run("")
	if strings.Contains(def, "dbg-line") || !strings.Contains(def, "info-line") {
		t.Errorf("default level wrong: %s", def)
	}
	parseLines(t, def)
	dbg := run("debug")
	if !strings.Contains(dbg, "dbg-line") || !strings.Contains(dbg, "info-line") {
		t.Errorf("RELAY_LOG_LEVEL=debug did not enable debug: %s", dbg)
	}
	parseLines(t, dbg)
}

func TestLogger_MultibyteTruncatesTo500Runes(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("é世", 300) // 600 runes
	newLogger(&buf, "info", fixedClock()).Info(long, "status", "error", "error", long)
	m := parseLines(t, buf.String())[0]
	for _, k := range []string{"msg", "error"} {
		s := m[k].(string)
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) != 500 {
			t.Errorf("%s: valid=%v runes=%d, want valid and 500", k, utf8.ValidString(s), utf8.RuneCountInString(s))
		}
	}
}

func TestLogger_ReservedCallerKeysAreRenamed(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "info", fixedClock())
	l.InfoContext(withTrace(context.Background(), "abcdef12"), "real",
		"service", "evil", "ts", "evil", "level", "evil", "msg", "evil", "trace_id", "evil")
	m := parseLines(t, buf.String())[0]
	for _, k := range []string{"service", "ts", "level", "msg", "trace_id"} {
		if m["attr_"+k] != "evil" {
			t.Errorf("attr_%s = %v, want evil", k, m["attr_"+k])
		}
	}
	if m["msg"] != "real" || m["trace_id"] != "abcdef12" || m["service"] == "evil" {
		t.Errorf("reserved keys overwritten: %v", m)
	}
}

func TestLogger_NonStringStatusBecomesHTTPStatus(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, "info", fixedClock()).Info("x", "status", 404)
	m := parseLines(t, buf.String())[0]
	if m["http_status"] != float64(404) {
		t.Errorf("http_status = %v, want 404", m["http_status"])
	}
}

func TestLogger_Defaults(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, "info", fixedClock()).Info("bare")
	m := parseLines(t, buf.String())[0]
	if m["status"] != "ok" || m["duration_ms"] != float64(0) || m["error"] != "" || m["trace_id"] != "" {
		t.Errorf("defaults wrong: %v", m)
	}
}

func TestLogger_DurationForms(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "info", fixedClock())
	l.Info("a", "duration_ms", 7)
	l.Info("b", "duration_ms", int64(8))
	l.Info("c", "duration_ms", 1500*time.Millisecond)
	got := []float64{}
	for _, m := range parseLines(t, buf.String()) {
		got = append(got, m["duration_ms"].(float64))
	}
	if fmt.Sprint(got) != "[7 8 1500]" {
		t.Errorf("duration_ms = %v, want [7 8 1500]", got)
	}
}

func TestLogger_MsgAndErrorTruncatedAt500(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("x", 900)
	newLogger(&buf, "info", fixedClock()).Info(long, "status", "error", "error", long)
	m := parseLines(t, buf.String())[0] // schema enforces maxLength 500
	if len(m["msg"].(string)) < 400 || len(m["error"].(string)) < 400 {
		t.Errorf("truncation too aggressive: msg=%d error=%d", len(m["msg"].(string)), len(m["error"].(string)))
	}
}

func TestLogger_TraceIDOnlyFromContext(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "info", fixedClock())
	l.Info("no ctx", "trace_id", "abcdef12")
	l.InfoContext(withTrace(context.Background(), "ctxtrace1"), "ctx")
	lines := parseLines(t, buf.String())
	if lines[0]["trace_id"] != "" || lines[0]["attr_trace_id"] != "abcdef12" {
		t.Errorf("caller trace_id not renamed: %v", lines[0])
	}
	if lines[1]["trace_id"] != "ctxtrace1" {
		t.Errorf("context trace_id lost: %v", lines[1])
	}
}
