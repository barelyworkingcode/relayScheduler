// Command devboxverify drives the installed relayScheduler through relay's
// front door against the devboxWorld test world and reports one result per
// journey. See README.md beside this file.
package main

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type state string

const (
	statePass    state = "PASS"
	stateFail    state = "FAIL"
	stateBlocked state = "BLOCKED"
	stateNotRun  state = "NOTRUN"
)

type result struct {
	ID     string
	State  state
	Detail string
}

// env is what a journey needs. Credential is never printed.
type env struct {
	FrontendSocket, Credential, RelayBin string
	ProjectID, ProjectName               string // world project "acme"
	Nonce                                string // 8 hex
}

type journey struct {
	ID      string
	Timeout time.Duration
	Run     func(ctx context.Context, e env) result
}

// worldProjectKey is the devboxWorld project the journeys run in.
const worldProjectKey = "acme"

// vmCheck is the machine check readMarker runs; a variable only so it reads as one named step.
var vmCheck = hostIsVM

var home, _ = os.UserHomeDir()

func main() { os.Exit(run()) }

func formatLine(home string, fields ...string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = strings.Join(strings.Fields(scrub(f, home)), " ")
	}
	return strings.Join(out, "\t")
}

func emit(fields ...string) { fmt.Println(formatLine(home, fields...)) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return strings.TrimSpace(string(out)), nil
}

func scrub(s, home string) string {
	if home == "" {
		return s
	}
	return strings.ReplaceAll(s, home, "~")
}

func tally(rs []result) (counts map[state]int, exitCode int) {
	counts = map[state]int{}
	for _, r := range rs {
		counts[r.State]++
	}
	if counts[stateFail] > 0 || counts[stateBlocked] > 0 || counts[stateNotRun] > 0 || counts[statePass] == 0 {
		exitCode = 1
	}
	return counts, exitCode
}

type preflightCheck struct {
	name string
	run  func() (string, error)
}

// runPreflight stops at the first FAIL; the caller exits 2.
func runPreflight(checks []preflightCheck) bool {
	for _, c := range checks {
		detail, err := c.run()
		if err != nil {
			emit("PREFLIGHT", c.name, "FAIL", err.Error())
			return false
		}
		emit("PREFLIGHT", c.name, "OK", detail)
	}
	return true
}

func parseFlags(fs *flag.FlagSet, args []string, pr *int) error {
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("unexpected arguments")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["post"] && *pr < 1 {
		return errors.New("--post needs a PR number")
	}
	return nil
}

func run() int {
	start := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fs := flag.NewFlagSet("devboxverify", flag.ContinueOnError)
	checkout := fs.String("checkout", "", "relayScheduler checkout the running service must be built from (default: this checkout)")
	pr := fs.Int("post", 0, "PR number to post the status and evidence comment to")
	if err := parseFlags(fs, os.Args[1:], pr); err != nil {
		fmt.Fprintln(os.Stderr, "usage: devboxverify [--checkout DIR] [--post PR]")
		return 2
	}
	toolRoot, err := gitOut(".", "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintln(os.Stderr, "run devboxverify from inside a relayScheduler checkout")
		return 2
	}
	toolCommit, _ := gitOut(toolRoot, "rev-parse", "HEAD")
	if *checkout == "" {
		*checkout = toolRoot
	}

	e := env{RelayBin: envOr("RELAY_BIN", "/Applications/Relay.app/Contents/MacOS/relay")}
	credFile := envOr("RELAYSCHEDULER_VERIFY_CREDENTIAL_FILE", filepath.Join(home, ".config", "relayScheduler-verify", "credential"))
	var marker worldMarker
	var head string
	checks := []preflightCheck{
		{"machine", func() (string, error) {
			marker, err = readMarker(markerPath(), vmCheck)
			return fmt.Sprintf("vm; world v%d", marker.WorldVersion), err
		}},
		{"session", func() (string, error) {
			if os.Getenv("RELAY_SESSION_ID") != "" {
				return "", errors.New("run from an operator shell, not a relay session")
			}
			return "operator shell", nil
		}},
		{"head", func() (string, error) {
			head, err = gitOut(*checkout, "rev-parse", "HEAD")
			return head, err
		}},
		{"build", func() (string, error) { return checkBuild(head) }},
		{"app", func() (string, error) {
			cfg, cerr := os.UserConfigDir()
			if cerr != nil {
				return "", errors.New("cannot find the user config dir")
			}
			var pid int
			e.FrontendSocket, pid, err = findApp(filepath.Join(cfg, "relay"))
			return fmt.Sprintf("pid %d", pid), err
		}},
		{"credential", func() (string, error) {
			e.Credential, err = readCredential(credFile)
			return "proxy-class credential read", err
		}},
		{"world", func() (string, error) {
			name, werr := worldProjectName(marker, worldProjectKey)
			if werr != nil {
				return "", errors.New("BLOCKED fixture: " + werr.Error())
			}
			gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			rs, gerr := grantRecords(gctx, e.RelayBin)
			if gerr != nil {
				return "", gerr
			}
			g := findGrant(rs, name)
			if g == nil || g.ID == "" {
				return "", fmt.Errorf("BLOCKED fixture: project %s is not granted in relay", name)
			}
			e.ProjectID, e.ProjectName = g.ID, name
			return "project " + name, nil
		}},
	}
	if *pr > 0 {
		checks = append(checks, preflightCheck{"pr", func() (string, error) {
			ph, perr := prHead(ctx, *pr)
			if perr != nil {
				return "", errors.New("gh pr view failed")
			}
			if ph != head {
				return "", fmt.Errorf("PR head %.12s is not the checkout HEAD", ph)
			}
			return "PR head is HEAD", nil
		}})
	}
	if !runPreflight(checks) {
		return 2
	}

	// Deliberate: the lock comes after preflight, so a refused machine
	// never queues for it, and it is released after the journey's cleanup.
	release, lerr := takeWorldLock(ctx)
	if lerr != nil {
		emit("PREFLIGHT", "lock", "FAIL", lerr.Error())
		return 2
	}
	defer release()
	emit("PREFLIGHT", "lock", "OK", "holding "+lockName)

	// The service can be rebuilt or restarted while this run queued for the
	// lock, so the build check is repeated once the lock is held.
	detail, berr := checkBuild(head)
	if berr != nil {
		emit("PREFLIGHT", "build", "FAIL", berr.Error())
		return 2 // deferred release runs
	}
	emit("PREFLIGHT", "build", "OK", detail+" (after lock)")

	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	e.Nonce = hex.EncodeToString(nonce)
	var results []result
	for _, j := range journeys {
		fmt.Fprintln(os.Stderr, "running", j.ID)
		began := time.Now()
		jctx, cancel := context.WithTimeout(ctx, j.Timeout)
		r := j.Run(jctx, e)
		cancel()
		results = append(results, r)
		emit("JOURNEY", r.ID, string(r.State), r.Detail)
		emit("TIMING", "journey", j.ID, strconv.FormatInt(time.Since(began).Milliseconds(), 10))
	}
	if ctx.Err() != nil {
		emit("SUMMARY", "interrupted")
		return 2
	}
	counts, code := tally(results)
	runTime := time.Since(start)
	emit("TIMING", "run", strconv.FormatInt(runTime.Milliseconds(), 10))
	emit("SUMMARY", fmt.Sprintf("pass=%d", counts[statePass]), fmt.Sprintf("fail=%d", counts[stateFail]),
		fmt.Sprintf("blocked=%d", counts[stateBlocked]), fmt.Sprintf("notrun=%d", counts[stateNotRun]))

	if *pr > 0 {
		ev := evidence{PR: *pr, Commit: head, ToolCommit: toolCommit, Home: home, RunTime: runTime, Results: results}
		url, perr := post(context.WithoutCancel(ctx), ev)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "post failed:", scrub(perr.Error(), home))
			return 2
		}
		emit("POSTED", statusState(results), url)
	}
	return code
}

// checkBuild requires exactly one running relayscheduler, built from head
// with a clean tree, and started after its binary was last written.
func checkBuild(head string) (string, error) {
	out, _ := exec.Command("pgrep", "-x", "relayscheduler").Output()
	pids := strings.Fields(string(out))
	if len(pids) != 1 {
		return "", fmt.Errorf("%d relayscheduler processes, want 1", len(pids))
	}
	pid := pids[0]
	comm, err := exec.Command("ps", "-ww", "-o", "comm=", "-p", pid).Output()
	path := strings.TrimSpace(string(comm))
	if err != nil || path == "" {
		return "", errors.New("cannot read the service's executable path")
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", errors.New("relayscheduler: no build info")
	}
	if err := buildMatches(info.Settings, head); err != nil {
		return "", fmt.Errorf("relayscheduler: %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", errors.New("cannot stat the service binary")
	}
	started, err := processStart(pid)
	if err != nil {
		return "", err
	}
	// A process that started before the binary was written is an older build
	// still running after a swap, even when its path matches.
	if started.Before(st.ModTime().Truncate(time.Second)) {
		return "", errors.New("the running service predates the installed binary")
	}
	return "service built from HEAD, clean tree", nil
}

func processStart(pid string) (time.Time, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", pid)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, errors.New("cannot read the service's start time")
	}
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		return time.Time{}, errors.New("cannot read the service's start time")
	}
	return t, nil
}

func buildMatches(settings []debug.BuildSetting, head string) error {
	var rev, modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case rev == "":
		return errors.New("no vcs.revision in build info")
	case rev != head:
		return fmt.Errorf("built from %.12s, not HEAD %.12s", rev, head)
	case modified != "false":
		return errors.New("built from a modified tree")
	}
	return nil
}

// findApp wants exactly one live relay-frontend-<pid>.sock in dir.
func findApp(dir string) (sock string, pid int, err error) {
	socks, _ := filepath.Glob(filepath.Join(dir, "relay-frontend-*.sock"))
	live := 0
	for _, s := range socks {
		var p int
		_, serr := fmt.Sscanf(filepath.Base(s), "relay-frontend-%d.sock", &p)
		if serr == nil && p > 0 && syscall.Kill(p, 0) != syscall.ESRCH {
			live++
			sock, pid = s, p
		}
	}
	if live != 1 {
		return "", 0, fmt.Errorf("%d live frontend sockets, want 1", live)
	}
	return sock, pid, nil
}
