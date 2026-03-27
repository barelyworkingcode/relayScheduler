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

	slog.Info("executing task", "task", task.Name, "projectId", task.ProjectID)

	// End previous session if one exists (one live session per task max).
	if current, err := s.store.Get(task.ID); err == nil && current != nil && current.LastSessionID != "" {
		s.client.EndSession(current.LastSessionID)
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

	// Create a headless session via relayLLM.
	session, err := s.client.CreateSession(task.ProjectID, model, task.Name)
	if err != nil {
		exec.Status = "error"
		exec.Error = err.Error()
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Error("task session creation failed", "task", task.Name, "error", err)
		s.logStore.Log(task.ProjectID, task.ID, exec)
		s.store.SetLastRun(task.ID, "error")
		s.hub.Broadcast(map[string]string{
			"type":      "task_error",
			"taskId":    task.ID,
			"projectId": task.ProjectID,
			"taskName":  task.Name,
			"error":     err.Error(),
		})
		s.rescheduleOrDisable(task)
		return
	}

	exec.SessionID = session.SessionID

	// Persist session ID immediately so page refreshes get the right value.
	s.store.SetLastSessionID(task.ID, session.SessionID)

	// Broadcast task_started now that we have a sessionId.
	s.hub.Broadcast(map[string]string{
		"type":      "task_started",
		"taskId":    task.ID,
		"projectId": task.ProjectID,
		"taskName":  task.Name,
		"sessionId": session.SessionID,
	})

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

	// Broadcast completion or error.
	if exec.Status == "error" {
		s.hub.Broadcast(map[string]string{
			"type":      "task_error",
			"taskId":    task.ID,
			"projectId": task.ProjectID,
			"taskName":  task.Name,
			"error":     exec.Error,
		})
	} else {
		s.hub.Broadcast(map[string]interface{}{
			"type":      "task_completed",
			"taskId":    task.ID,
			"projectId": task.ProjectID,
			"taskName":  task.Name,
			"sessionId": session.SessionID,
			"status":    exec.Status,
		})
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

