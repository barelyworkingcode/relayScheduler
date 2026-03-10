package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// RegisterRoutes sets up the HTTP API for task management.
func RegisterRoutes(mux *http.ServeMux, scheduler *Scheduler, logStore *LogStore) {
	// GET /api/tasks — list all scheduled tasks.
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		writeJSON(w, 200, scheduler.GetAllTasks())
	})

	// GET /api/tasks/:projectId/:taskId/history — task execution history.
	// POST /api/tasks/:projectId/:taskId/run — run task now.
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		parts := strings.SplitN(path, "/", 3)

		if len(parts) < 2 {
			w.WriteHeader(404)
			return
		}

		projectID := parts[0]
		taskID := parts[1]

		if len(parts) == 3 {
			switch parts[2] {
			case "history":
				if r.Method != http.MethodGet {
					w.WriteHeader(405)
					return
				}
				history := logStore.Load(projectID, taskID)
				writeJSON(w, 200, history)
				return

			case "run":
				if r.Method != http.MethodPost {
					w.WriteHeader(405)
					return
				}
				go scheduler.RunTaskNow(projectID, taskID)
				writeJSON(w, 200, map[string]string{
					"success": "true",
					"message": "Task execution started",
				})
				return
			}
		}

		w.WriteHeader(404)
	})
}
