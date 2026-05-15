package main

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const writeWait = 10 * time.Second

// Hub manages WebSocket connections and broadcasts task events to all clients.
type Hub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
	store   *TaskStore
}

func NewHub(store *TaskStore) *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]struct{}),
		store:   store,
	}
}

func (h *Hub) Register(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Unregister(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

// Broadcast sends a JSON message to all connected clients.
// Unregisters clients that fail to receive.
func (h *Hub) Broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("hub: failed to marshal broadcast", "error", err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for conn := range h.clients {
		conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			slog.Warn("hub: write failed, removing client", "error", err)
			conn.Close()
			delete(h.clients, conn)
		}
	}
}

// SendStatus sends a task_status snapshot of all currently running tasks to a single connection.
func (h *Hub) SendStatus(conn *websocket.Conn) {
	tasks, err := h.store.Load()
	if err != nil {
		slog.Error("hub: failed to load tasks for status", "error", err)
		return
	}

	type runningTask struct {
		TaskID    string   `json:"taskId"`
		ProjectID string   `json:"projectId"`
		TaskName  string   `json:"taskName"`
		View      TaskView `json:"view"`
	}

	var running []runningTask
	for _, t := range tasks {
		if t.LastStatus != "running" {
			continue
		}
		runID := t.LastSessionID
		if t.SessionType == SessionTypePTY {
			runID = t.LastTerminalID
		}
		running = append(running, runningTask{
			TaskID:    t.ID,
			ProjectID: t.ProjectID,
			TaskName:  t.Name,
			View:      taskView(t, runID),
		})
	}

	msg := map[string]interface{}{
		"type":    "task_status",
		"running": running,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	h.mu.Lock()
	conn.SetWriteDeadline(time.Now().Add(writeWait))
	conn.WriteMessage(websocket.TextMessage, data)
	h.mu.Unlock()
}
