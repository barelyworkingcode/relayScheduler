package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Relay logging standard: one JSON object per line on stderr, nine required
// keys on every line. See relay docs/logging-standard.md.

const (
	defaultServiceID = "relay-scheduler"
	debugWindow      = 30 * time.Minute
	maxLogText       = 500
	traceHeader      = "X-Trace-Id"
)

var traceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type traceKey struct{}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS is broken; no safe fallback.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func validTraceID(s string) bool { return traceIDPattern.MatchString(s) }

func withTrace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceKey{}, id)
}

func traceFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}

// traceMiddleware keeps a caller-supplied trace id only when it is well
// formed; a rejected value is replaced and never logged.
func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(traceHeader)
		if !validTraceID(id) {
			id = newTraceID()
		}
		next.ServeHTTP(w, r.WithContext(withTrace(r.Context(), id)))
	})
}

func initLogging() {
	slog.SetDefault(newLogger(os.Stderr, os.Getenv("RELAY_LOG_LEVEL"), time.Now))
}

func newLogger(w io.Writer, level string, now func() time.Time) *slog.Logger {
	if now == nil {
		now = time.Now
	}
	svc := os.Getenv("RELAY_SERVICE_ID")
	if svc == "" {
		svc = defaultServiceID
	}
	st := &logState{w: w, now: now, service: svc, level: slog.LevelInfo}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error":
		st.level = slog.LevelError
	case "warn":
		st.level = slog.LevelWarn
	case "debug":
		st.level = slog.LevelDebug
		st.debugSince = now()
		st.debugOn = true
	}
	return slog.New(&relayHandler{st: st})
}

// logState is shared by every handler derived from one logger.
type logState struct {
	mu         sync.Mutex
	w          io.Writer
	now        func() time.Time
	service    string
	level      slog.Level
	debugOn    bool
	debugSince time.Time
}

type kv struct {
	key string
	val slog.Value
}

type relayHandler struct {
	st     *logState
	prefix string
	attrs  []kv
}

func (h *relayHandler) Enabled(_ context.Context, l slog.Level) bool {
	st := h.st
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.debugOn && !st.now().Before(st.debugSince.Add(debugWindow)) {
		st.debugOn = false
		st.level = slog.LevelInfo
		st.write(map[string]any{
			"ts": st.stamp(st.now()), "level": "warn",
			"msg":     "debug logging ended after 30 minutes; level is now info",
			"service": st.service, "op": "log.level", "status": "ok",
			"duration_ms": 0, "error": "", "trace_id": "",
		})
	}
	return l >= st.level
}

func (h *relayHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]kv(nil), h.attrs...), flatten(h.prefix, as)...)
	return &n
}

func (h *relayHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.prefix = h.prefix + name + "."
	return &n
}

func flatten(prefix string, as []slog.Attr) []kv {
	var out []kv
	for _, a := range as {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			p := prefix
			if a.Key != "" {
				p += a.Key + "."
			}
			out = append(out, flatten(p, v.Group())...)
			continue
		}
		if a.Key == "" {
			continue
		}
		out = append(out, kv{prefix + a.Key, v})
	}
	return out
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

func clip(s string) string {
	if len(s) <= maxLogText {
		return s
	}
	r := []rune(s)
	if len(r) > maxLogText {
		r = r[:maxLogText]
	}
	s = string(r)
	for len(s) > maxLogText { // multibyte: limit is characters, but keep it conservative
		r = r[:len(r)-1]
		s = string(r)
	}
	return s
}

func (st *logState) stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// write must be called with st.mu held.
func (st *logState) write(m map[string]any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return
	}
	_, _ = st.w.Write(buf.Bytes())
}

func durationMS(v slog.Value) (int64, bool) {
	switch v.Kind() {
	case slog.KindInt64:
		return v.Int64(), true
	case slog.KindUint64:
		return int64(v.Uint64()), true
	case slog.KindDuration:
		return v.Duration().Milliseconds(), true
	case slog.KindAny:
		switch x := v.Any().(type) {
		case int:
			return int64(x), true
		case int32:
			return int64(x), true
		}
	}
	return 0, false
}

func (h *relayHandler) Handle(ctx context.Context, r slog.Record) error {
	st := h.st
	lvl := levelName(r.Level)
	status := "ok"
	if lvl == "error" || lvl == "warn" {
		status = "error"
	}
	m := map[string]any{
		"ts":          st.stamp(r.Time),
		"level":       lvl,
		"msg":         clip(r.Message),
		"service":     st.service,
		"op":          "log",
		"status":      status,
		"duration_ms": int64(0),
		"error":       "",
		"trace_id":    traceFrom(ctx),
	}
	if r.Time.IsZero() {
		m["ts"] = st.stamp(st.now())
	}
	apply := func(k kv) {
		switch k.key {
		case "ts", "level", "msg", "service", "trace_id":
			m["attr_"+k.key] = k.val.Any()
		case "op":
			m["op"] = k.val.String()
		case "status":
			if k.val.Kind() == slog.KindString {
				m["status"] = k.val.String()
			} else {
				m["http_status"] = k.val.Any()
			}
		case "duration_ms":
			if ms, ok := durationMS(k.val); ok {
				m["duration_ms"] = ms
			}
		case "error":
			m["error"] = clip(fmt.Sprint(k.val.Any()))
		default:
			m[k.key] = k.val.Any()
		}
	}
	for _, k := range h.attrs {
		apply(k)
	}
	r.Attrs(func(a slog.Attr) bool {
		for _, k := range flatten(h.prefix, []slog.Attr{a}) {
			apply(k)
		}
		return true
	})
	st.mu.Lock()
	defer st.mu.Unlock()
	st.write(m)
	return nil
}
