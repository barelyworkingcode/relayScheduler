package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

const envHelperBootstrap = "RELAYSCHEDULER_TEST_HELPER_BOOTSTRAP"

// TestBootstrapLaunchIdentity_RealFD3 launches this test binary the way relay
// launches the service: the secret on a pipe passed as ExtraFiles[0] (fd 3)
// with RELAY_LAUNCH_FD=3, against a real Unix-socket bridge.
func TestBootstrapLaunchIdentity_RealFD3(t *testing.T) {
	cases := map[string]struct {
		pipe    string
		reply   func(map[string]json.RawMessage) string
		wantOK  bool
		wantOut string
	}{
		"success":   {pipe: testSecret, reply: okHello("svc-1", 4242), wantOK: true, wantOut: "BOOTSTRAP_OK"},
		"badSecret": {pipe: testSecret + "\n", reply: okHello("svc-1", 4242)},
		"refused": {pipe: testSecret, reply: func(map[string]json.RawMessage) string {
			return `{"type":"Error","code":-32001,"message":"hello refused"}`
		}},
		"wrongIDBack": {pipe: testSecret, reply: okHello("other", 4242)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fb := startFakeBridge(t, tc.reply)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			w.WriteString(tc.pipe)
			w.Close()

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperBootstrap$")
			cmd.ExtraFiles = []*os.File{r}
			cmd.Env = append(os.Environ(),
				envHelperBootstrap+"=1",
				envLaunchFD+"=3",
				envBridgeSocket+"="+fb.path,
				envServiceID+"=svc-1",
				"RELAY_SERVICE_TOKEN=stale",
				"RELAY_FRONTEND_TOKEN=stale",
			)
			out, err := cmd.CombinedOutput()
			r.Close()

			if strings.Contains(string(out), testSecret) {
				t.Fatalf("helper output contains the secret:\n%s", out)
			}
			if tc.wantOK {
				if err != nil || !strings.Contains(string(out), tc.wantOut) {
					t.Fatalf("helper failed: %v\n%s", err, out)
				}
			} else if err == nil {
				t.Fatalf("helper exited zero on a failed bootstrap:\n%s", out)
			}
		})
	}
}

// TestHelperBootstrap is the child half of TestBootstrapLaunchIdentity_RealFD3
// and does nothing in a normal run.
func TestHelperBootstrap(t *testing.T) {
	if os.Getenv(envHelperBootstrap) != "1" {
		t.Skip("helper process only")
	}
	var before syscall.Stat_t
	if err := syscall.Fstat(launchFD, &before); err != nil {
		t.Fatalf("fd 3 not open before bootstrap: %v", err)
	}
	id, err := bootstrapLaunchIdentity()
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if id == nil || id.serviceID != "svc-1" || id.relayPID != 4242 {
		t.Fatalf("identity = %+v", id)
	}
	for _, name := range append([]string{envLaunchFD}, removedCredentialEnv...) {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s still set", name)
		}
	}
	// The runtime reuses fd 3 once the pipe is closed (the netpoller's own
	// wakeup pipe lands there on the Hello dial), so only the same pipe
	// object still on fd 3 means the launch pipe leaked.
	var after syscall.Stat_t
	if err := syscall.Fstat(launchFD, &after); err == nil && after.Dev == before.Dev && after.Ino == before.Ino {
		t.Fatal("launch pipe still open on fd 3 after bootstrap")
	}
	os.Stdout.WriteString("BOOTSTRAP_OK\n")
}
