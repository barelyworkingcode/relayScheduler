package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// validateTask enforces what a runnable task needs. Chat tasks need Prompt and
// Model; PTY tasks need TemplateID. An empty SessionType is treated as
// "headless" for task records that predate PTY support.
func validateTask(task Task) error {
	if task.Name == "" {
		return errors.New("name is required")
	}
	if task.ProjectID == "" {
		return errors.New("projectId is required")
	}
	if len(task.Schedule) == 0 {
		return errors.New("schedule is required")
	}
	if err := ValidateSchedule(task.Schedule); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	switch task.SessionType {
	case SessionTypePTY:
		if task.TemplateID == "" {
			return errors.New("templateId is required for PTY tasks")
		}
	case "", SessionTypeChat:
		if task.Prompt == "" {
			return errors.New("prompt is required for chat tasks")
		}
		if strings.TrimSpace(task.Model) == "" {
			return fmt.Errorf("task %q: model is required for chat tasks", task.Name)
		}
	default:
		return fmt.Errorf("invalid sessionType %q (expected \"headless\" or \"pty\")", task.SessionType)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// decodeTask reads a task body and validates it, writing a 400 on failure.
// POST and PUT share it so an update can never store a task that create
// would have rejected.
func decodeTask(w http.ResponseWriter, r *http.Request) (Task, bool) {
	var task Task
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return Task{}, false
	}
	if err := validateTask(task); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return Task{}, false
	}
	return task, true
}

// RegisterRoutes mounts the task API. The mux answers 405 for a known path
// with the wrong method.
func RegisterRoutes(mux *http.ServeMux, store *TaskStore, scheduler *Scheduler, logStore *LogStore) {
	mux.HandleFunc("GET /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var tasks []Task
		var err error
		if projectID := r.URL.Query().Get("projectId"); projectID != "" {
			tasks, err = store.ListByProject(projectID)
		} else {
			tasks, err = store.Load()
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tasks == nil {
			tasks = []Task{}
		}
		writeJSON(w, http.StatusOK, tasks)
	})

	mux.HandleFunc("POST /api/tasks", func(w http.ResponseWriter, r *http.Request) {
		task, ok := decodeTask(w, r)
		if !ok {
			return
		}
		created, err := store.Create(task)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		scheduler.ScheduleTask(*created)
		writeJSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, err := store.Get(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if task == nil {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, task)
	})

	mux.HandleFunc("PUT /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		updated, ok := decodeTask(w, r)
		if !ok {
			return
		}
		task, err := store.Update(r.PathValue("id"), updated)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if task == nil {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		scheduler.ScheduleTask(*task)
		writeJSON(w, http.StatusOK, task)
	})

	mux.HandleFunc("DELETE /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		scheduler.UnscheduleTask(id)
		deleted, err := store.Delete(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !deleted {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})

	mux.HandleFunc("GET /api/tasks/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		task, err := store.Get(r.PathValue("id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if task == nil {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, logStore.Load(task.ProjectID, task.ID))
	})

	mux.HandleFunc("POST /api/tasks/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		err := scheduler.RunTaskNow(r.PathValue("id"))
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"message": "Task execution started",
			})
		case errors.Is(err, ErrTaskRunning):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrTaskNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
	})

	mux.HandleFunc("DELETE /api/tasks/by-project/{projectId}", func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("projectId")
		scheduler.UnscheduleByProject(projectID)
		count, err := store.DeleteByProject(projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": count})
	})
}
