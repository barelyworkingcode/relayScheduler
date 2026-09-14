package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func pipeWith(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return r
}

func TestReadLaunchSecret_Valid(t *testing.T) {
	r := pipeWith(t, testSecret)
	got, err := readLaunchSecret(r)
	if err != nil {
		t.Fatalf("readLaunchSecret: %v", err)
	}
	if got != testSecret {
		t.Fatalf("secret mismatch")
	}
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Fatal("pipe read end still open after readLaunchSecret")
	}
}

func TestReadLaunchSecret_Rejects(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"short":           testSecret[:63],
		"long":            testSecret + "0",
		"trailingNewline": testSecret + "\n",
		"uppercase":       strings.ToUpper(testSecret),
		"nonHex":          "g" + testSecret[1:],
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			r := pipeWith(t, content)
			_, err := readLaunchSecret(r)
			if err == nil {
				t.Fatal("accepted invalid secret")
			}
			if content != "" && strings.Contains(err.Error(), content) {
				t.Fatal("error echoes pipe contents")
			}
			if _, err := r.Read(make([]byte, 1)); err == nil {
				t.Fatal("pipe read end still open after rejection")
			}
		})
	}
}

// fakeBridge is a real Unix-socket server answering one line per connection
// with reply(request). Requests are recorded raw.
type fakeBridge struct {
	path string
	mu   sync.Mutex
	reqs []map[string]json.RawMessage
}

func startFakeBridge(t *testing.T, reply func(req map[string]json.RawMessage) string) *fakeBridge {
	t.Helper()
	// Short dir: macOS caps sun_path at 104 bytes and t.TempDir() can exceed it.
	dir, err := os.MkdirTemp("", "rsb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	fb := &fakeBridge{path: filepath.Join(dir, "b.sock")}
	ln, err := net.Listen("unix", fb.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				if !sc.Scan() {
					return
				}
				var req map[string]json.RawMessage
				_ = json.Unmarshal(sc.Bytes(), &req)
				fb.mu.Lock()
				fb.reqs = append(fb.reqs, req)
				fb.mu.Unlock()
				if out := reply(req); out != "" {
					c.Write([]byte(out + "\n"))
				}
			}(conn)
		}
	}()
	return fb
}

func (fb *fakeBridge) requests() []map[string]json.RawMessage {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), fb.reqs...)
}

func okHello(serviceID string, pid int) func(map[string]json.RawMessage) string {
	return func(map[string]json.RawMessage) string {
		b, _ := json.Marshal(map[string]any{
			"type": "OK",
			"data": map[string]any{"kind": "service", "service_id": serviceID, "relay_pid": pid, "future": true},
		})
		return string(b)
	}
}

func TestSendHello_Success(t *testing.T) {
	fb := startFakeBridge(t, okHello("svc-1", 4242))
	pid, err := sendHello(fb.path, "svc-1", testSecret)
	if err != nil {
		t.Fatalf("sendHello: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("relay pid = %d, want 4242", pid)
	}
	reqs := fb.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	var typ, name, token string
	json.Unmarshal(reqs[0]["type"], &typ)
	json.Unmarshal(reqs[0]["name"], &name)
	json.Unmarshal(reqs[0]["token"], &token)
	if typ != "Hello" || name != "svc-1" || token != testSecret {
		t.Fatalf("hello frame = type %q name %q token-matches %v", typ, name, token == testSecret)
	}
}

func TestSendHello_Failures(t *testing.T) {
	cases := map[string]func(map[string]json.RawMessage) string{
		"refused": func(map[string]json.RawMessage) string {
			return `{"type":"Error","code":-32001,"message":"hello refused"}`
		},
		"malformed":      func(map[string]json.RawMessage) string { return `{not json` },
		"noReply":        func(map[string]json.RawMessage) string { return "" },
		"wrongServiceID": okHello("someone-else", 4242),
		"zeroPID":        okHello("svc-1", 0),
		"missingData":    func(map[string]json.RawMessage) string { return `{"type":"OK"}` },
		"unexpectedType": func(map[string]json.RawMessage) string { return `{"type":"Pong"}` },
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			fb := startFakeBridge(t, reply)
			_, err := sendHello(fb.path, "svc-1", testSecret)
			if err == nil {
				t.Fatal("sendHello succeeded")
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Fatal("error contains the secret")
			}
		})
	}
}

func TestSendHello_Unreachable(t *testing.T) {
	_, err := sendHello(filepath.Join(t.TempDir(), "absent.sock"), "svc-1", testSecret)
	if err == nil {
		t.Fatal("sendHello to missing socket succeeded")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("error contains the secret")
	}
}

func TestBootstrapLaunchIdentity_StandaloneWhenUnset(t *testing.T) {
	t.Setenv(envLaunchFD, "")
	os.Unsetenv(envLaunchFD)
	t.Setenv("RELAY_FRONTEND_TOKEN", "standalone-token")
	id, err := bootstrapLaunchIdentity()
	if err != nil || id != nil {
		t.Fatalf("bootstrap = %v, %v; want nil, nil", id, err)
	}
	if os.Getenv("RELAY_FRONTEND_TOKEN") != "standalone-token" {
		t.Fatal("standalone bootstrap scrubbed RELAY_FRONTEND_TOKEN")
	}
}

// Only a non-3 value is exercised: bootstrapping with "3" would read and
// close whatever the test binary has on fd 3.
func TestBootstrapLaunchIdentity_FailsClosedAndScrubs(t *testing.T) {
	t.Setenv(envLaunchFD, "7")
	for _, name := range removedCredentialEnv {
		t.Setenv(name, "stale")
	}
	id, err := bootstrapLaunchIdentity()
	if err == nil || id != nil {
		t.Fatalf("bootstrap = %v, %v; want error", id, err)
	}
	for _, name := range append([]string{envLaunchFD}, removedCredentialEnv...) {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("%s still set after bootstrap", name)
		}
	}
}

func TestRegisterManifest_WireHasNoToken(t *testing.T) {
	fb := startFakeBridge(t, func(map[string]json.RawMessage) string { return `{"type":"OK"}` })
	maybeRegisterManifest(&launchIdentity{serviceID: "svc-1", bridgeSocket: fb.path, relayPID: 1}, "/tmp/sched.sock", "internal-bearer")

	reqs := fb.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if _, ok := reqs[0]["token"]; ok {
		t.Fatal("RegisterManifest carries a token key")
	}
	var typ string
	json.Unmarshal(reqs[0]["type"], &typ)
	var args registerManifestRequest
	if err := json.Unmarshal(reqs[0]["arguments"], &args); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	if typ != reqRegisterManifest || args.ServiceID != "svc-1" || args.InternalSocket != "/tmp/sched.sock" {
		t.Fatalf("frame = %s %+v", typ, args)
	}
}

func TestRegisterManifest_StandaloneSkips(t *testing.T) {
	fb := startFakeBridge(t, func(map[string]json.RawMessage) string { return `{"type":"OK"}` })
	t.Setenv(envBridgeSocket, fb.path)
	maybeRegisterManifest(nil, "/tmp/sched.sock", "internal-bearer")
	time.Sleep(50 * time.Millisecond)
	if n := len(fb.requests()); n != 0 {
		t.Fatalf("standalone sent %d bridge requests", n)
	}
}

// TestFrontendClient_LaunchedSendsNoAuthorization drives the client over a
// real Unix socket, the way it runs under relay, for both HTTP and /ws.
func TestFrontendClient_LaunchedSendsNoAuthorization(t *testing.T) {
	dir, err := os.MkdirTemp("", "rsf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "f.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var authHeaders []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		_, present := r.Header["Authorization"]
		mu.Unlock()
		if present {
			http.Error(w, "unexpected Authorization", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/api/projects/p1":
			json.NewEncoder(w).Encode(Project{ID: "p1", Path: "/work"})
		case "/ws":
			http.Error(w, "no ws here", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	client := newFrontendClient(&launchIdentity{serviceID: "svc-1", relayPID: 1}, "http://localhost:3000", sock, "")
	p, err := client.GetProject("p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.Path != "/work" {
		t.Fatalf("project = %+v", p)
	}
	_, _ = client.dialWS()

	mu.Lock()
	defer mu.Unlock()
	if len(authHeaders) < 2 {
		t.Fatalf("server saw %d requests, want HTTP + ws", len(authHeaders))
	}
	for i, h := range authHeaders {
		if h != "" {
			t.Fatalf("request %d carried Authorization", i)
		}
	}
}

func TestNewFrontendClient_LaunchedIgnoresExplicitToken(t *testing.T) {
	c := newFrontendClient(&launchIdentity{serviceID: "svc-1", relayPID: 1}, "http://localhost:3000", "/tmp/x.sock", "explicit")
	if c.token != "" {
		t.Fatal("launched client kept a frontend bearer")
	}
	c = newFrontendClient(nil, "http://localhost:3000", "/tmp/x.sock", "explicit")
	if c.token != "explicit" {
		t.Fatal("standalone client dropped its explicit bearer")
	}
}
