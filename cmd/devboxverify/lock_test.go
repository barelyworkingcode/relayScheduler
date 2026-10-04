package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDevlock installs a shell script as DEVLOCK_BIN that logs its argv and
// then runs script.
func fakeDevlock(t *testing.T, script string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	bin := filepath.Join(dir, "devlock")
	body := "#!/bin/sh\necho \"$@\" >> '" + logPath + "'\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVLOCK_BIN", bin)
	return logPath
}

func TestWorldLockTakeAndRelease(t *testing.T) {
	logPath := fakeDevlock(t, "exit 0")
	release, err := takeWorldLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	raw, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls = %q", lines)
	}
	for _, want := range []string{"take WORLD", "--holder relayScheduler-verify", "--wait"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("take %q lacks %q", lines[0], want)
		}
	}
	if lines[1] != "release WORLD --holder relayScheduler-verify" {
		t.Errorf("release = %q", lines[1])
	}
}

func TestWorldLockTakeFailureReturnsError(t *testing.T) {
	logPath := fakeDevlock(t, "echo held by someone else; exit 1")
	release, err := takeWorldLock(context.Background())
	if err == nil || release != nil {
		t.Fatalf("err=%v release-nil=%v", err, release == nil)
	}
	if !strings.Contains(err.Error(), "held by someone else") {
		t.Errorf("error lacks devlock output: %v", err)
	}
	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), "release") {
		t.Error("released a lock that was never taken")
	}
}

func TestWorldLockWaitIsCapped(t *testing.T) {
	fakeDevlock(t, "exec tail -f /dev/null")
	old := lockWait
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	start := time.Now()
	_, err := takeWorldLock(context.Background())
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}
