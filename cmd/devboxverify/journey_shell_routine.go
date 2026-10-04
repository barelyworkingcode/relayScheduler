package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const shellRoutineID = "scheduled-shell-routine-runs"

var journeys = []journey{{shellRoutineID, 120 * time.Second, runShellRoutine}}

// Test seams only.
var (
	runDeadline = 60 * time.Second
	pollEvery   = time.Second
)

// historyEntry is the part of an Execution this journey reads. Response is
// the last 16 KB of the terminal log; Output is a different field (the
// outputFile contents) and is not used here.
type historyEntry struct {
	Status     string `json:"status"`
	ExitCode   *int   `json:"exitCode"`
	Response   string `json:"response"`
	Error      string `json:"error"`
	TerminalID string `json:"terminalId"`
}

func markerFor(nonce string) string { return "relayscheduler-verify-" + nonce }

// runShellRoutine creates a terminal routine, runs it once, waits for the
// history entry a finished run leaves, and checks exit code and output. The
// routine is deleted whatever the outcome.
func runShellRoutine(ctx context.Context, e env) (res result) {
	id := shellRoutineID
	name := "verify-" + e.Nonce + "-shell"
	marker := markerFor(e.Nonce)

	create := frontendDo(ctx, e, http.MethodPost, "/api/tasks", map[string]any{
		"name":        name,
		"projectId":   e.ProjectID,
		"schedule":    map[string]string{"type": "on_demand"},
		"enabled":     true,
		"catchUp":     false,
		"sessionType": "pty",
		"templateId":  "world-probe",
		"extraArgs":   []string{"-c", "printf 'relayscheduler-verify-%s\\n' " + e.Nonce},
	})
	if r, bad := stepRefusal(id, "create", create, http.StatusCreated); bad {
		return r
	}
	var created struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(create.Body, &created) != nil || created.ID == "" {
		return result{id, stateFail, "create: status 201 but no task id"}
	}
	taskPath := "/api/tasks/" + url.PathEscape(created.ID)
	defer func() { res = cleanupRoutine(e, res, name, taskPath) }()

	began := time.Now()
	if r, bad := stepRefusal(id, "run", frontendDo(ctx, e, http.MethodPost, taskPath+"/run", nil), http.StatusOK); bad {
		return r
	}

	hist, ended, pollNote := waitForHistory(ctx, e, taskPath, began.Add(runDeadline))
	if !ended {
		d := fmt.Sprintf("run did not end within %ds", int(runDeadline/time.Second))
		if pollNote != "" {
			d += "; " + pollNote
		}
		return result{id, stateFail, d}
	}
	r := classifyShellRun(true, hist, marker)
	if r.State == statePass {
		r.Detail += fmt.Sprintf(", %ds", int(time.Since(began).Round(time.Second)/time.Second))
	}
	if r.State == stateBlocked {
		r.Detail = strings.ReplaceAll(r.Detail, "<project>", e.ProjectName)
	}
	return r
}

// stepRefusal maps a front-door answer that is not the wanted status.
func stepRefusal(id, step string, r frontendResponse, want int) (result, bool) {
	switch {
	case r.Status == want:
		return result{}, false
	case r.Status == 0:
		return result{id, stateBlocked, "frontend socket unreachable"}, true
	case r.Status == http.StatusUnauthorized:
		return result{id, stateBlocked, "credential refused (401)"}, true
	case r.Status == http.StatusForbidden:
		return result{id, stateBlocked, "credential lacks the proxy class (403)"}, true
	}
	return result{id, stateFail, fmt.Sprintf("%s: status %d: %s", step, r.Status, r.Error)}, true
}

// waitForHistory polls until the history has an entry (written only when a
// run ends) or the deadline passes. A poll that fails is retried; the last
// failure is returned so a deadline miss can name it.
func waitForHistory(ctx context.Context, e env, taskPath string, deadline time.Time) (h historyEntry, ended bool, note string) {
	for {
		r := frontendDo(ctx, e, http.MethodGet, taskPath+"/history", nil)
		switch {
		case r.Status == http.StatusOK:
			var entries []historyEntry
			if err := json.Unmarshal(r.Body, &entries); err != nil {
				note = "history unreadable"
			} else if len(entries) > 0 {
				return entries[0], true, ""
			} else {
				note = ""
			}
		default:
			note = fmt.Sprintf("last history poll: status %d", r.Status)
		}
		if ctx.Err() != nil || !time.Now().Add(pollEvery).Before(deadline) {
			return historyEntry{}, false, note
		}
		select {
		case <-ctx.Done():
			return historyEntry{}, false, note
		case <-time.After(pollEvery):
		}
	}
}

// classifyShellRun judges the newest history entry; the first match wins.
// marker is the full line the routine prints.
func classifyShellRun(ended bool, h historyEntry, marker string) result {
	id := shellRoutineID
	switch {
	case !ended:
		return result{id, stateFail, fmt.Sprintf("run did not end within %ds", int(runDeadline/time.Second))}
	case h.Status == "error" && strings.Contains(h.Error, `template "world-probe"`):
		return result{id, stateBlocked, "world-probe not available to <project>; see prerequisites"}
	case h.ExitCode == nil:
		return result{id, stateFail, fmt.Sprintf("run ended %s with no exit code", h.Status)}
	case *h.ExitCode != 0:
		first, _, _ := strings.Cut(h.Error, "\n")
		return result{id, stateFail, fmt.Sprintf("run ended %s, exit %d: %s", h.Status, *h.ExitCode, first)}
	case h.Status != "success":
		return result{id, stateFail, fmt.Sprintf("run ended %s, exit 0", h.Status)}
	}
	for _, line := range strings.Split(h.Response, "\n") {
		if strings.TrimSpace(line) == marker {
			return result{id, statePass, "exit 0, marker in output"}
		}
	}
	return result{id, stateFail, "exit 0 but output lacks the marker"}
}

// cleanupRoutine runs after every outcome, on a context the run's
// cancellation cannot stop. A routine left behind turns PASS into FAIL and is
// named on any other result.
func cleanupRoutine(e env, res result, name, taskPath string) result {
	ctx := context.WithoutCancel(context.Background())
	if g := frontendDo(ctx, e, http.MethodGet, taskPath, nil); g.Status == http.StatusOK {
		var t struct {
			LastTerminalID string `json:"lastTerminalId"`
		}
		if json.Unmarshal(g.Body, &t) == nil && t.LastTerminalID != "" {
			_ = frontendDo(ctx, e, http.MethodDelete, "/api/terminals/"+url.PathEscape(t.LastTerminalID), nil)
		}
	}
	del := frontendDo(ctx, e, http.MethodDelete, taskPath, nil)
	after := frontendDo(ctx, e, http.MethodGet, taskPath, nil)
	if del.Status == http.StatusOK && after.Status == http.StatusNotFound {
		return res
	}
	status := del.Status
	if del.Status == http.StatusOK {
		status = after.Status
	}
	left := fmt.Sprintf("left routine %s: %d", name, status)
	if res.State == statePass {
		return result{res.ID, stateFail, left}
	}
	res.Detail += "; " + left
	return res
}
