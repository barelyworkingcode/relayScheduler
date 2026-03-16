package main

import (
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// HandleWS returns an HTTP handler that upgrades connections to WebSocket.
func HandleWS(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("ws upgrade failed", "error", err)
			return
		}

		hub.Register(conn)
		slog.Info("ws client connected", "remote", r.RemoteAddr)

		// Send current running task snapshot.
		hub.SendStatus(conn)

		// Read pump: discard incoming messages, detect disconnect.
		go func() {
			defer func() {
				hub.Unregister(conn)
				conn.Close()
				slog.Info("ws client disconnected", "remote", r.RemoteAddr)
			}()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}
}
