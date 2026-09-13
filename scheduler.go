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

// Scheduler fires tasks from a wall-clock ticker and runs them through relay.
// The ticker is resilient to macOS sleep because each tick checks the real
// wall clock rather than relying on monotonic-clock timers.
type Scheduler struct {
	mu       sync.Mutex
	client   *RelayClient
	store    *TaskStore
	logStore *LogStore
	hub      *Hub
	tasks    map[string]*scheduledTask // key: taskID
	running  map[string]struct{}       // taskIDs currently executing
	done     chan struct{}
}

func NewScheduler(client *RelayClient, store *TaskStore, logStore *LogStore, hub *Hub) *Scheduler {
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
	// Check immediately so tasks already due at startup don't wait a tick.
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

// missedThreshold is how late a run may be and still fire when CatchUp is off.
const missedThreshold = 10 * time.Minute

func (s *Scheduler) checkAndFireTasks() {
	now := time.Now()

	s.mu.Lock()
	var toFire []Task
	var toSkip []Task

	for _, st := range s.tasks {
		if !now.Before(st.nextRun) { // nextRun <= now
			if _, ok := s.running[st.task.ID]; ok {
				continue // already executing (e.g. manual trigger)
			}
			overdue := now.Sub(st.nextRun)
			if !st.task.CatchUp && overdue > missedThreshold {
				toSkip = append(toSkip, st.task)
			} else {
				toFire = append(toFire, st.task)
			}
		}
	}

	// Drop due tasks from the map. rescheduleOrDisable (fired) and the loop
	// below (skipped) re-add them with their next run.
	for _, task := range toFire {
		delete(s.tasks, task.ID)
	}
	for _, task := range toSkip {
		delete(s.tasks, task.ID)
	}

	for _, task := range toSkip {
		// A skipped one-shot has no next occurrence. Disable it rather than
		// leave it enabled but never scheduled.
		if st, _ := ScheduleType(task.Schedule); st == "once" {
			slog.Info("disabling missed one-shot task (catch-up disabled)", "task", task.Name)
			s.store.SetEnabled(task.ID, false)
			continue
		}
		slog.Info("skipping missed task (catch-up disabled)", "task", task.Name)
		s.scheduleTaskLocked(task)
	}
	for _, task := range toFire {
		s.running[task.ID] = struct{}{}
	}
	s.mu.Unlock()

	for _, task := range toFire {
		go s.executeTask(task)
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

func (s *Scheduler) LoadAllTasks() error {
	tasks, err := s.store.Load()
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tasks = make(map[string]*scheduledTask)

	for _, task := range tasks {
		// A "running" status at startup means the previous process died mid-run.
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

func (s *Scheduler) ScheduleTask(task Task) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.tasks, task.ID)

	if !task.Enabled {
		return
	}

	s.scheduleTaskLocked(task)
}

func (s *Scheduler) UnscheduleTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskID)
}

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

	// One live run per task: remove the previous run's session or terminal
	// before starting a new one.
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

	// Resolve the project so we can pass `directory` + `projectId` to relayLLM
	// — relayLLM is a pure execution engine and has no project awareness; relay
	// brokers the scoped token from the projectId.
	project, err := s.client.GetProject(task.ProjectID)
	if err != nil {
		s.failRun(task, exec, err)
		return
	}

	if task.SessionType == SessionTypePTY {
		s.runPtyTask(task, project, exec)
		return
	}

	session, err := s.client.CreateSession(project, task.Model, task.Name)
	if err != nil {
		s.failRun(task, exec, err)
		return
	}

	exec.SessionID = session.SessionID

	// Persist session ID immediately so page refreshes get the right value.
	s.store.SetLastSessionID(task.ID, session.SessionID)

	s.broadcastTaskEvent("task_started", task, session.SessionID, nil)

	timeout := time.Duration(task.MaxDurationSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultChatTimeout
	}
	result, err := s.client.RunChatAndWait(session.SessionID, task.Prompt, timeout)
	exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	switch {
	case errors.Is(err, ErrChatTimeout):
		exec.Status = "timeout"
		exec.Error = "task exceeded maxDurationSeconds"
		// Stop the still-running generation so it doesn't linger until the
		// provider's own idle timeout.
		s.client.StopGeneration(session.SessionID)
		slog.Warn("task timed out", "task", task.Name, "timeout", timeout)
	case err != nil:
		exec.Status = "error"
		exec.Error = err.Error()
		slog.Error("task execution failed", "task", task.Name, "error", err)
	default:
		exec.Status = "success"
		exec.Response = result.Response
		exec.Stats = &result.Stats
		slog.Info("task completed", "task", task.Name,
			"tokens", result.Stats.InputTokens+result.Stats.OutputTokens)
	}

	// The session stays alive so eve can open this run; the next run deletes it.

	s.logStore.Log(task.ProjectID, task.ID, exec)
	s.store.SetLastRun(task.ID, exec.Status)

	if exec.Status == "success" {
		s.broadcastTaskEvent("task_completed", task, session.SessionID, map[string]interface{}{"status": exec.Status})
	} else {
		s.broadcastTaskEvent("task_error", task, session.SessionID, map[string]interface{}{"error": exec.Error, "status": exec.Status})
	}

	s.rescheduleOrDisable(task)
}

// failRun records a run that failed before its session or terminal existed.
// PTY runs get ExitCodeCreateFailed so their history always carries an exit code.
func (s *Scheduler) failRun(task Task, exec Execution, err error) {
	exec.Status = "error"
	exec.Error = err.Error()
	exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	extra := map[string]interface{}{"error": exec.Error}
	if task.SessionType == SessionTypePTY {
		exitCode := ExitCodeCreateFailed
		exec.ExitCode = &exitCode
		extra["exitCode"] = exitCode
	}
	slog.Error("task failed to start", "task", task.Name, "sessionType", task.SessionType, "error", err)
	s.logStore.Log(task.ProjectID, task.ID, exec)
	s.store.SetLastRun(task.ID, "error")
	s.broadcastTaskEvent("task_error", task, "", extra)
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

// Stop halts the ticker and clears the schedule. In-flight runs finish on
// their own.
func (s *Scheduler) Stop() {
	close(s.done)
	s.mu.Lock()
	s.tasks = make(map[string]*scheduledTask)
	s.running = make(map[string]struct{})
	s.mu.Unlock()
}

// defaultPtyTimeout caps a PTY run when MaxDurationSeconds is unset.
// Long-running daemons must opt in to a longer cap via that field.
const defaultPtyTimeout = 30 * time.Minute

// defaultChatTimeout caps a chat task's run when MaxDurationSeconds is unset.
// Generous because local models can be slow over many tool calls; the WS stream
// has no relayLLM-side cap, so this is the only bound.
const defaultChatTimeout = 30 * time.Minute

// broadcastTaskEvent sends a lifecycle event with the standard envelope (type,
// taskId, projectId, taskName, view) plus event-specific extras such as
// status, error, and exitCode. Every broadcast goes through here so the view
// envelope stays consistent.
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
		s.failRun(task, exec, errors.New("PTY task missing templateId"))
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
		s.failRun(task, exec, err)
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
