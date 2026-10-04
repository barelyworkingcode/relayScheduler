package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	lockName   = "WORLD"
	lockHolder = "relayScheduler-verify"
)

// lockWait caps how long the run waits for WORLD; a test seam only.
var lockWait = 15 * time.Minute

func devlockBin() string { return envOr("DEVLOCK_BIN", "devlock") }

// takeWorldLock waits for the WORLD lock and returns its release. The lease
// is 10 minutes, longer than one journey run.
func takeWorldLock(ctx context.Context) (release func(), err error) {
	bin := devlockBin()
	wctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	cmd := exec.CommandContext(wctx, bin, "take", lockName, "--holder", lockHolder,
		"--minutes", "10", "--reason", "relayScheduler devbox/verify", "--wait")
	cmd.WaitDelay = 5 * time.Second
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = runErr.Error()
		}
		if errors.Is(wctx.Err(), context.DeadlineExceeded) {
			detail = fmt.Sprintf("waited %s for %s; %s", lockWait, lockName, detail)
		}
		return nil, errors.New(detail)
	}
	return func() { releaseWorldLock(bin) }, nil
}

// releaseWorldLock uses a fresh context: a cancelled run must still let go.
func releaseWorldLock(bin string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "release", lockName, "--holder", lockHolder)
	cmd.WaitDelay = 5 * time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "devlock release %s failed: %s\n", lockName, strings.TrimSpace(string(out)))
	}
}
