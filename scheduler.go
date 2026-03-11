package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const tasksFilename = ".tasks.json"

type scheduledTask struct {
	projectID   string
	projectName string
	task        Task
	timer       *time.Timer
	nextRun     time.Time
}

// Scheduler loads tasks from project directories and runs them on schedule.
type Scheduler struct {
	mu       sync.Mutex
	client   *LLMClient
	logStore *LogStore
	tasks    map[string]*scheduledTask // key: "projectId:taskId"
	watcher  *fsnotify.Watcher
	projects []Project
	running  bool
}

func NewScheduler(client *LLMClient, logStore *LogStore) *Scheduler {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create file watcher", "error", err)
	}

	s := &Scheduler{
		client:   client,
		logStore: logStore,
		tasks:    make(map[string]*scheduledTask),
		watcher:  w,
		running:  true,
	}

	if w != nil {
		go s.watchLoop()
	}
	return s
}

// LoadProjects fetches the project list from relayLLM and loads tasks.
func (s *Scheduler) LoadProjects() error {
	projects, err := s.client.ListProjects()
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.projects = projects
	s.mu.Unlock()

	for _, p := range projects {
		s.loadProjectTasks(p)
	}
	return nil
}

func (s *Scheduler) loadProjectTasks(project Project) {
	tasksPath := filepath.Join(project.Path, tasksFilename)
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("failed to read tasks file", "project", project.Name, "error", err)
		}
		return
	}

	var tf TaskFile
	if err := json.Unmarshal(data, &tf); err != nil {
		slog.Error("failed to parse tasks file", "project", project.Name, "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Clear existing tasks for this project.
	for key, st := range s.tasks {
		if st.projectID == project.ID {
			st.timer.Stop()
			delete(s.tasks, key)
		}
	}

	// Schedule enabled tasks.
	for _, task := range tf.Tasks {
		if !task.Enabled {
			continue
		}
		s.scheduleTaskLocked(project, task)
	}

	// Watch the project directory for .tasks.json changes.
	if s.watcher != nil {
		s.watcher.Add(project.Path)
	}

	slog.Info("loaded tasks", "project", project.Name, "count", len(tf.Tasks))
}

func (s *Scheduler) scheduleTaskLocked(project Project, task Task) {
	nextRun, err := CalculateNextRun(task.Schedule)
	if err != nil {
		slog.Error("failed to calculate next run", "task", task.Name, "error", err)
		return
	}

	delay := time.Until(nextRun)
	if delay < 0 {
		delay = 0
	}

	key := project.ID + ":" + task.ID
	st := &scheduledTask{
		projectID:   project.ID,
		projectName: project.Name,
		task:        task,
		nextRun:     nextRun,
	}

	st.timer = time.AfterFunc(delay, func() {
		s.executeTask(project, task)
	})

	s.tasks[key] = st
	slog.Info("scheduled task", "task", task.Name, "project", project.Name, "nextRun", nextRun.Format(time.RFC3339))
}

func (s *Scheduler) executeTask(project Project, task Task) {
	slog.Info("executing task", "task", task.Name, "project", project.Name)

	exec := Execution{
		TaskID:      task.ID,
		TaskName:    task.Name,
		ProjectID:   project.ID,
		ProjectName: project.Name,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Status:      "running",
	}

	// Determine model: task override or project default.
	model := task.Model
	if model == "" {
		model = project.Model
	}

	// Create a headless session via relayLLM.
	session, err := s.client.CreateSession(project.ID, model)
	if err != nil {
		exec.Status = "error"
		exec.Error = err.Error()
		exec.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		slog.Error("task session creation failed", "task", task.Name, "error", err)
		s.logStore.Log(project.ID, task.ID, exec)
		s.reschedule(project, task)
		return
	}

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
		slog.Info("task completed", "task", task.Name, "project", project.Name,
			"tokens", result.Stats.InputTokens+result.Stats.OutputTokens)
	}

	// Clean up the session.
	s.client.EndSession(session.SessionID)

	// Log the execution.
	s.logStore.Log(project.ID, task.ID, exec)

	// Reschedule if still running.
	s.reschedule(project, task)
}

func (s *Scheduler) reschedule(project Project, task Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.scheduleTaskLocked(project, task)
	}
}

func (s *Scheduler) watchLoop() {
	// Debounce per project directory: batch changes within 100ms.
	debounceTimers := make(map[string]*time.Timer)

	for {
		select {
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != tasksFilename {
				continue
			}
			projectDir := filepath.Dir(event.Name)
			if t, ok := debounceTimers[projectDir]; ok {
				t.Stop()
			}
			debounceTimers[projectDir] = time.AfterFunc(100*time.Millisecond, func() {
				s.reloadProjectByPath(projectDir)
			})

		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("file watcher error", "error", err)
		}
	}
}

func (s *Scheduler) reloadProjectByPath(projectDir string) {
	s.mu.Lock()
	projects := s.projects
	s.mu.Unlock()

	for _, p := range projects {
		if p.Path == projectDir {
			slog.Info("reloading tasks", "project", p.Name)
			s.loadProjectTasks(p)
			return
		}
	}
}

// Stop cancels all scheduled tasks and closes the file watcher.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	s.running = false
	for _, st := range s.tasks {
		st.timer.Stop()
	}
	s.tasks = make(map[string]*scheduledTask)
	s.mu.Unlock()

	if s.watcher != nil {
		s.watcher.Close()
	}
}

// RunTaskNow finds and executes a task immediately, bypassing the schedule.
func (s *Scheduler) RunTaskNow(projectID, taskID string) {
	s.mu.Lock()
	var project Project
	for _, p := range s.projects {
		if p.ID == projectID {
			project = p
			break
		}
	}
	s.mu.Unlock()

	if project.ID == "" {
		slog.Error("project not found for manual run", "projectId", projectID)
		return
	}

	// Read the task from the tasks file.
	tasksPath := filepath.Join(project.Path, tasksFilename)
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		slog.Error("failed to read tasks file for manual run", "error", err)
		return
	}

	var tf TaskFile
	if err := json.Unmarshal(data, &tf); err != nil {
		slog.Error("failed to parse tasks file for manual run", "error", err)
		return
	}

	for _, task := range tf.Tasks {
		if task.ID == taskID {
			s.executeTask(project, task)
			return
		}
	}

	slog.Error("task not found for manual run", "projectId", projectID, "taskId", taskID)
}

// GetAllTasks returns all loaded tasks with their next run times.
func (s *Scheduler) GetAllTasks() []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]map[string]interface{}, 0)
	for _, st := range s.tasks {
		result = append(result, map[string]interface{}{
			"id":          st.task.ID,
			"name":        st.task.Name,
			"projectId":   st.projectID,
			"projectName": st.projectName,
			"enabled":     st.task.Enabled,
			"nextRun":     st.nextRun.Format(time.RFC3339),
			"schedule":    st.task.Schedule,
		})
	}
	return result
}
