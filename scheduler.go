package main

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	ErrTaskNotFound = errors.New("task not found")
	ErrTaskRunning  = errors.New("task is already running")
)

type scheduledTask struct {
	task    Task
	nextRun time.Time
}

// Scheduler manages task scheduling via a wall-clock ticker and executes
// tasks via the LLM client. The ticker approach is resilient to macOS sleep
// because each tick checks the real wall clock rather than relying on
// monotonic-clock timers.
type Scheduler struct {
	mu       sync.Mutex
	client   *LLMClient
	store    *TaskStore
	logStore *LogStore
	hub      *Hub
	tasks    map[string]*scheduledTask // key: taskID
	running  map[string]struct{}       // taskIDs currently executing
	done     chan struct{}
}

func NewScheduler(client *LLMClient, store *TaskStore, logStore *LogStore, hub *Hub) *Scheduler {
	return &Scheduler{
		client:   client,
		store:    store,
		logStore: logStore,
		hub:      hub,
		tasks:    make(map[string]*scheduledTask),
		running:  make(map[string]struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the wall-clock ticker loop. Call after LoadAllTasks.
func (s *Scheduler) Start() {
	go s.tickLoop()
}

func (s *Scheduler) tickLoop() {
	// Check immediately on start for any already-due tasks.
	s.checkAndFireTasks()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.checkAndFireTasks()
		}
	}
}

const missedThreshold = 10 * time.Minute

func (s *Scheduler) checkAndFireTasks() {
	now := time.Now()

	s.mu.Lock()
	var toFire []Task
	var toReschedule []Task

	for _, st := range s.tasks {
		if !now.Before(st.nextRun) { // nextRun <= now
			if _, ok := s.running[st.task.ID]; ok {
				continue // already executing (e.g. manual trigger)
			}
			overdue := now.Sub(st.nextRun)
			if !st.task.CatchUp && overdue > missedThreshold {
				// Missed and catch-up disabled: skip, reschedule for next occurrence.
				toReschedule = append(toReschedule, st.task)
			} else {
				toFire = append(toFire, st.task)
			}
		}
	}

	// Remove fired/rescheduled tasks from the map.
	// rescheduleOrDisable (for fired) and scheduleTaskLocked (for skipped)
	// will re-add them with the next nextRun.
	for _, task := range toFire {
		delete(s.tasks, task.ID)
	}
	for _, task := range toReschedule {
		delete(s.tasks, task.ID)
	}

	// Reschedule skipped tasks while still under lock.
	for _, task := range toReschedule {
		slog.Info("skipping missed task (catch-up disabled)", "task", task.Name)
		s.scheduleTaskLocked(task)
	}
	for _, task := range toFire {
		s.running[task.ID] = struct{}{}
	}
	s.mu.Unlock()

	// Execute each due task in its own goroutine.
	for _, task := range toFire {
		taskCopy := task
		go s.executeTask(taskCopy)
	}
}

func (s *Scheduler) isStopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// LoadAllTasks reads all tasks from the store and schedules enabled ones.
func (s *Scheduler) LoadAllTasks() error {
	tasks, err := s.store.Load()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tasks = make(map[string]*scheduledTask)

	for _, task := range tasks {
		// Reset stale "running" status from crash recovery.
		if task.LastStatus == "running" {
			slog.Warn("resetting stale running task", "task", task.Name, "id", task.ID)
			s.store.SetLastRun(task.ID, "error")
		}
		if !task.Enabled {
			continue
		}
		s.scheduleTaskLocked(task)
	}

	slog.Info("loaded tasks from store", "total", len(tasks), "scheduled", len(s.tasks))
	return nil
}

// ScheduleTask schedules (or reschedules) a single task.
func (s *Scheduler) ScheduleTask(task Task) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.tasks, task.ID)

	if !task.Enabled {
		return
	}

	s.scheduleTaskLocked(task)
}

// UnscheduleTask removes a task from the schedule.
func (s *Scheduler) UnscheduleTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
}

// UnscheduleByProject removes all tasks for a project from the schedule.
func (s *Scheduler) UnscheduleByProject(projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, st := range s.tasks {
		if st.task.ProjectID == projectID {
			delete(s.tasks, id)
		}
	}
}

func (s *Scheduler) scheduleTaskLocked(task Task) {
	// on_demand tasks only run via RunTaskNow; never auto-schedule them.
	st, _ := ScheduleType(task.Schedule)
	if st == "on_demand" {
		return
	}

	nextRun, err := CalculateNextRun(task.Schedule)
	if err != nil {
		slog.Error("failed to calculate next run", "task", task.Name, "error", err)
		return
	}

	s.tasks[task.ID] = &scheduledTask{
		task:    task,
		nextRun: nextRun,
	}
	slog.Info("scheduled task", "task", task.Name, "nextRun", nextRun.UTC().Format(time.RFC3339))
}

func (s *Scheduler) executeTask(task Task) {
	defer func() {
		s.mu.Lock()
		delete(s.running, task.ID)
		s.mu.Unlock()
	}()

	slog.Info("executing task", "task", task.Name, "projectId", task.ProjectID, "sessionType", task.SessionType)

	// Delete previous session if one exists (one live session per task max).
	// Dispatches by SessionType so a PTY task closes its previous terminal
	// rather than trying to delete a non-existent chat session.
	if current, err := s.store.Get(task.ID); err == nil && current != nil {
		switch current.SessionType {
		case SessionTypePTY:
			if current.LastTerminalID != "" {
				s.client.CloseTerminal(current.LastTerminalID)
			}
		default:
			if current.LastSessionID != "" {
				s.client.DeleteSession(current.LastSessionID)
			}
		}
	}

	exec := Execution{
		TaskID:    task.ID,
		TaskName:  task.Name,
		ProjectID: task.ProjectID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "running",
	}

	// Mark task as running so clients can detect in-progress execution.
	s.store.SetLastRun(task.ID, "running")

	model := task.Model

	// Resolve the project so we can pass `directory` and `mcpToken` to relayLLM
	// — relayLLM is a pure execution engine and has no project awareness.
	project, err := s.client.GetProject(task.ProjectID)
	if err != nil {
		exec.Status = "error"
		exec.Error = err.Error()
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Error("task project lookup failed", "task", task.Name, "error", err)
		s.logStore.Log(task.ProjectID, task.ID, exec)
		s.store.SetLastRun(task.ID, "error")
		s.broadcastTaskEvent("task_error", task, "", map[string]interface{}{"error": err.Error()})
		s.rescheduleOrDisable(task)
		return
	}

	// PTY tasks take a different path — no LLM session, just a terminal.
	if task.SessionType == SessionTypePTY {
		s.runPtyTask(task, project, exec)
		return
	}

	// Create a headless session via relayLLM (proxied through relay).
	session, err := s.client.CreateSession(project, model, task.Name)
	if err != nil {
		exec.Status = "error"
		exec.Error = err.Error()
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Error("task session creation failed", "task", task.Name, "error", err)
		s.logStore.Log(task.ProjectID, task.ID, exec)
		s.store.SetLastRun(task.ID, "error")
		s.broadcastTaskEvent("task_error", task, "", map[string]interface{}{"error": err.Error()})
		s.rescheduleOrDisable(task)
		return
	}

	exec.SessionID = session.SessionID

	// Persist session ID immediately so page refreshes get the right value.
	s.store.SetLastSessionID(task.ID, session.SessionID)

	s.broadcastTaskEvent("task_started", task, session.SessionID, nil)

	// Send the prompt and wait for the response.
	result, err := s.client.SendMessage(session.SessionID, task.Prompt)
	if err != nil {
		exec.Status = "error"
		exec.Error = err.Error()
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Error("task execution failed", "task", task.Name, "error", err)
	} else {
		exec.Status = "success"
		exec.Response = result.Response
		exec.Stats = &result.Stats
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Info("task completed", "task", task.Name,
			"tokens", result.Stats.InputTokens+result.Stats.OutputTokens)
	}

	// Keep session alive for click-to-join (no EndSession call).

	// Log the execution.
	s.logStore.Log(task.ProjectID, task.ID, exec)

	// Update last run status in store.
	s.store.SetLastRun(task.ID, exec.Status)

	if exec.Status == "error" {
		s.broadcastTaskEvent("task_error", task, session.SessionID, map[string]interface{}{"error": exec.Error})
	} else {
		s.broadcastTaskEvent("task_completed", task, session.SessionID, map[string]interface{}{"status": exec.Status})
	}

	// Reschedule or disable.
	s.rescheduleOrDisable(task)
}

func (s *Scheduler) rescheduleOrDisable(task Task) {
	// Re-read from store to get the latest version. The task may have been
	// updated, deleted, or disabled via API during execution.
	current, err := s.store.Get(task.ID)
	if err != nil || current == nil || !current.Enabled {
		return
	}
	task = *current

	st, _ := ScheduleType(task.Schedule)

	if st == "once" {
		s.store.SetEnabled(task.ID, false)
		s.mu.Lock()
		delete(s.tasks, task.ID)
		s.mu.Unlock()
		slog.Info("disabled one-shot task after execution", "task", task.Name)
		return
	}

	if st == "on_demand" {
		// on_demand tasks stay enabled; they only run via RunTaskNow.
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isStopped() {
		s.scheduleTaskLocked(task)
	}
}

// Stop cancels all scheduled tasks and stops the ticker.
func (s *Scheduler) Stop() {
	close(s.done)
	s.mu.Lock()
	s.tasks = make(map[string]*scheduledTask)
	s.running = make(map[string]struct{})
	s.mu.Unlock()
}

// defaultPtyTimeout is the wall-clock cap applied when a PTY task does not
// set MaxDurationSeconds. Long-running daemons must opt in via that field.
const defaultPtyTimeout = 30 * time.Minute

// broadcastTaskEvent sends a task lifecycle WS event with the standard
// envelope (type, taskId, projectId, taskName, view). Extras are merged in
// for event-specific fields like `status`, `error`, `exitCode`. Use the
// helper instead of hand-building the map so the view envelope stays
// consistent across every broadcast site.
func (s *Scheduler) broadcastTaskEvent(eventType string, task Task, runID string, extra map[string]interface{}) {
	msg := map[string]interface{}{
		"type":      eventType,
		"taskId":    task.ID,
		"projectId": task.ProjectID,
		"taskName":  task.Name,
		"view":      taskView(task, runID),
	}
	for k, v := range extra {
		msg[k] = v
	}
	s.hub.Broadcast(msg)
}

// runPtyTask is the PTY-mode mirror of the chat path. It launches a terminal
// session on relayLLM, attaches via WS to learn the exit code, captures the
// log tail for the execution record, and logs/broadcasts completion.
//
// The starting `exec` already has TaskID/TaskName/ProjectID/StartedAt set
// and Status="running" from executeTask.
func (s *Scheduler) runPtyTask(task Task, project *Project, exec Execution) {
	if task.TemplateID == "" {
		s.failPtyTask(task, exec, "PTY task missing templateId", ExitCodeCreateFailed)
		return
	}

	directory := project.Path
	if task.Directory != "" {
		directory = task.Directory
	}
	// CreateTerminal pulls Directory from project.Path; honor the per-task
	// override by passing a shallow copy with Path adjusted.
	projectForTerminal := *project
	projectForTerminal.Path = directory

	term, err := s.client.CreateTerminal(&projectForTerminal, task.TemplateID, task.Name, task.ExtraArgs)
	if err != nil {
		s.failPtyTask(task, exec, err.Error(), ExitCodeCreateFailed)
		return
	}

	exec.TerminalID = term.ID
	s.store.SetLastTerminalID(task.ID, term.ID)

	s.broadcastTaskEvent("task_started", task, term.ID, nil)

	timeout := time.Duration(task.MaxDurationSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultPtyTimeout
	}

	exitCode, attachErr := s.client.AttachTerminalAndWait(term.ID, timeout)
	exec.ExitCode = &exitCode
	exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)

	// On timeout the PTY is still alive — reap it before reading the log,
	// otherwise the child can keep writing bytes during the read and we'd
	// capture a racy/partial snapshot.
	if exitCode == ExitCodeTimeout {
		s.client.CloseTerminal(term.ID)
	}

	// Log preview for the execution record. Eve renders the full stream
	// via /api/terminals/{id}/log; this is just a quick glance for the
	// history list. Best-effort: never fail the run on a read error.
	if logBytes, lerr := s.client.GetTerminalLog(term.ID); lerr == nil {
		const maxPreview = 16 * 1024
		if len(logBytes) > maxPreview {
			logBytes = logBytes[len(logBytes)-maxPreview:]
		}
		exec.Response = string(logBytes)
	}

	switch {
	case exitCode == ExitCodeTimeout:
		exec.Status = "timeout"
		exec.Error = "task exceeded maxDurationSeconds"
	case exitCode < 0:
		exec.Status = "error"
		if attachErr != nil {
			exec.Error = attachErr.Error()
		}
	case exitCode == 0:
		exec.Status = "success"
	default:
		exec.Status = "error"
		exec.Error = fmt.Sprintf("process exited with code %d", exitCode)
	}

	s.logStore.Log(task.ProjectID, task.ID, exec)
	s.store.SetLastRun(task.ID, exec.Status)

	extra := map[string]interface{}{"status": exec.Status, "exitCode": exitCode}
	if exec.Status == "success" {
		s.broadcastTaskEvent("task_completed", task, term.ID, extra)
	} else {
		extra["error"] = exec.Error
		s.broadcastTaskEvent("task_error", task, term.ID, extra)
	}

	s.rescheduleOrDisable(task)
}

// failPtyTask records a PTY-mode failure that occurred before the WS attach
// (typically template missing or POST /api/terminals returned an error).
func (s *Scheduler) failPtyTask(task Task, exec Execution, msg string, exitCode int) {
	exec.Status = "error"
	exec.Error = msg
	exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	exec.ExitCode = &exitCode
	slog.Error("pty task failed", "task", task.Name, "error", msg)
	s.logStore.Log(task.ProjectID, task.ID, exec)
	s.store.SetLastRun(task.ID, "error")
	s.broadcastTaskEvent("task_error", task, "", map[string]interface{}{"error": msg, "exitCode": exitCode})
	s.rescheduleOrDisable(task)
}

// RunTaskNow executes a task immediately, bypassing the schedule.
func (s *Scheduler) RunTaskNow(taskID string) error {
	task, err := s.store.Get(taskID)
	if err != nil {
		return err
	}
	if task == nil {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}

	s.mu.Lock()
	if _, ok := s.running[taskID]; ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrTaskRunning, taskID)
	}
	s.running[taskID] = struct{}{}
	s.mu.Unlock()

	go s.executeTask(*task)
	return nil
}

