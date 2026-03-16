package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// RegisterRoutes sets up the HTTP API for task management.
func RegisterRoutes(mux *http.ServeMux, store *TaskStore, scheduler *Scheduler, logStore *LogStore) {
	// GET /api/tasks — list tasks (optional ?projectId= filter).
	// POST /api/tasks — create task.
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			projectID := r.URL.Query().Get("projectId")
			var tasks []Task
			var err error
			if projectID != "" {
				tasks, err = store.ListByProject(projectID)
			} else {
				tasks, err = store.List()
			}
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if tasks == nil {
				tasks = []Task{}
			}
			writeJSON(w, 200, tasks)

		case http.MethodPost:
			var task Task
			if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
				writeError(w, 400, "invalid JSON: "+err.Error())
				return
			}
			if task.Name == "" || task.Prompt == "" || task.ProjectID == "" || len(task.Schedule) == 0 {
				writeError(w, 400, "name, prompt, projectId, and schedule are required")
				return
			}
			created, err := store.Create(task)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			scheduler.ScheduleTask(*created)
			writeJSON(w, 201, created)

		default:
			w.WriteHeader(405)
		}
	})

	// Routes under /api/tasks/
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		parts := strings.SplitN(path, "/", 2)

		if len(parts) == 0 || parts[0] == "" {
			w.WriteHeader(404)
			return
		}

		// DELETE /api/tasks/by-project/{projectId}
		if parts[0] == "by-project" {
			if r.Method != http.MethodDelete {
				w.WriteHeader(405)
				return
			}
			if len(parts) < 2 || parts[1] == "" {
				writeError(w, 400, "projectId required")
				return
			}
			projectID := parts[1]
			scheduler.UnscheduleByProject(projectID)
			count, err := store.DeleteByProject(projectID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]interface{}{
				"deleted": count,
			})
			return
		}

		taskID := parts[0]

		// Sub-resources: /api/tasks/{taskId}/history, /api/tasks/{taskId}/run
		if len(parts) == 2 {
			switch parts[1] {
			case "history":
				if r.Method != http.MethodGet {
					w.WriteHeader(405)
					return
				}
				task, err := store.Get(taskID)
				if err != nil {
					writeError(w, 500, err.Error())
					return
				}
				if task == nil {
					writeError(w, 404, "task not found")
					return
				}
				history := logStore.Load(task.ProjectID, taskID)
				writeJSON(w, 200, history)
				return

			case "run":
				if r.Method != http.MethodPost {
					w.WriteHeader(405)
					return
				}
				if err := scheduler.RunTaskNow(taskID); err != nil {
					writeError(w, 500, err.Error())
					return
				}
				writeJSON(w, 200, map[string]interface{}{
					"success": true,
					"message": "Task execution started",
				})
				return
			}

			w.WriteHeader(404)
			return
		}

		// Single task operations: GET, PUT, DELETE /api/tasks/{taskId}
		switch r.Method {
		case http.MethodGet:
			task, err := store.Get(taskID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if task == nil {
				writeError(w, 404, "task not found")
				return
			}
			writeJSON(w, 200, task)

		case http.MethodPut:
			var updated Task
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				writeError(w, 400, "invalid JSON: "+err.Error())
				return
			}
			task, err := store.Update(taskID, updated)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if task == nil {
				writeError(w, 404, "task not found")
				return
			}
			scheduler.ScheduleTask(*task)
			writeJSON(w, 200, task)

		case http.MethodDelete:
			scheduler.UnscheduleTask(taskID)
			deleted, err := store.Delete(taskID)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if !deleted {
				writeError(w, 404, "task not found")
				return
			}
			writeJSON(w, 200, map[string]interface{}{"deleted": true})

		default:
			w.WriteHeader(405)
		}
	})
}
