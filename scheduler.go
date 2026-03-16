package main

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

type scheduledTask struct {
	task    Task
	timer   *time.Timer
	nextRun time.Time
}

// Scheduler manages task timers and executes tasks via the LLM client.
type Scheduler struct {
	mu       sync.Mutex
	client   *LLMClient
	store    *TaskStore
	logStore *LogStore
	hub      *Hub
	tasks    map[string]*scheduledTask // key: taskID
	running  bool
}

func NewScheduler(client *LLMClient, store *TaskStore, logStore *LogStore, hub *Hub) *Scheduler {
	return &Scheduler{
		client:   client,
		store:    store,
		logStore: logStore,
		hub:      hub,
		tasks:    make(map[string]*scheduledTask),
		running:  true,
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

	// Cancel existing timers.
	for _, st := range s.tasks {
		st.timer.Stop()
	}
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

	// Cancel existing timer if any.
	if st, ok := s.tasks[task.ID]; ok {
		st.timer.Stop()
		delete(s.tasks, task.ID)
	}

	if !task.Enabled {
		return
	}

	s.scheduleTaskLocked(task)
}

// UnscheduleTask cancels the timer for a task.
func (s *Scheduler) UnscheduleTask(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st, ok := s.tasks[taskID]; ok {
		st.timer.Stop()
		delete(s.tasks, taskID)
	}
}

// UnscheduleByProject cancels all timers for tasks belonging to a project.
func (s *Scheduler) UnscheduleByProject(projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, st := range s.tasks {
		if st.task.ProjectID == projectID {
			st.timer.Stop()
			delete(s.tasks, id)
		}
	}
}

func (s *Scheduler) scheduleTaskLocked(task Task) {
	nextRun, err := CalculateNextRun(task.Schedule)
	if err != nil {
		slog.Error("failed to calculate next run", "task", task.Name, "error", err)
		return
	}

	delay := time.Until(nextRun)
	if delay < 0 {
		delay = 0
	}

	taskCopy := task
	st := &scheduledTask{
		task:    taskCopy,
		nextRun: nextRun,
	}

	st.timer = time.AfterFunc(delay, func() {
		s.executeTask(taskCopy)
	})

	s.tasks[task.ID] = st
	slog.Info("scheduled task", "task", task.Name, "nextRun", nextRun.Format(time.RFC3339))
}

func (s *Scheduler) executeTask(task Task) {
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

	// Update last run status and session ID in store.
	s.store.SetLastRun(task.ID, exec.Status)
	s.store.SetLastSessionID(task.ID, session.SessionID)

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
	// For once/immediate: disable instead of rescheduling.
	var base struct {
		Type string `json:"type"`
	}
	json.Unmarshal(task.Schedule, &base)

	if base.Type == "once" || base.Type == "on_demand" {
		s.store.SetEnabled(task.ID, false)
		s.mu.Lock()
		delete(s.tasks, task.ID)
		s.mu.Unlock()
		slog.Info("disabled one-shot task after execution", "task", task.Name)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.scheduleTaskLocked(task)
	}
}

// Stop cancels all scheduled tasks.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.running = false
	for _, st := range s.tasks {
		st.timer.Stop()
	}
	s.tasks = make(map[string]*scheduledTask)
	s.mu.Unlock()
}

// RunTaskNow executes a task immediately, bypassing the schedule.
func (s *Scheduler) RunTaskNow(taskID string) error {
	task, err := s.store.Get(taskID)
	if err != nil {
		return err
	}
	if task == nil {
		slog.Error("task not found for manual run", "taskId", taskID)
		return nil
	}
	go s.executeTask(*task)
	return nil
}

// GetAllTasks returns all scheduled tasks with their next run times.
func (s *Scheduler) GetAllTasks() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]map[string]interface{}, 0)
	for _, st := range s.tasks {
		result = append(result, map[string]interface{}{
			"id":        st.task.ID,
			"name":      st.task.Name,
			"projectId": st.task.ProjectID,
			"enabled":   st.task.Enabled,
			"nextRun":   st.nextRun.Format(time.RFC3339),
			"schedule":  st.task.Schedule,
		})
	}
	return result
}
