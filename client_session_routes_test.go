package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newSessionRoutesStandIn is a stand-in mirroring relay main's session route
// table (relay internal/sessions/hostapi/server.go, and the stop_generation
// frame in internal/sessions/api/ws_session.go). It registers only the routes
// relay main serves; anything else 404s and is reported on unknown.
func newSessionRoutesStandIn(t *testing.T) (srv *httptest.Server, deleted chan string, frames chan map[string]any, unknown chan string) {
	t.Helper()
	deleted = make(chan string, 8)
	frames = make(chan map[string]any, 8)
	unknown = make(chan string, 8)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted <- r.PathValue("id")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/sessions/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f map[string]any
			if json.Unmarshal(data, &f) == nil {
				frames <- f
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		unknown <- r.Method + " " + r.URL.Path
		http.NotFound(w, r)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return
}

func TestDeleteSession_UsesRelayMainDeleteRoute(t *testing.T) {
	srv, deleted, _, unknown := newSessionRoutesStandIn(t)
	client := NewRelayClient(srv.URL, "", "")

	client.DeleteSession("s1")

	select {
	case id := <-deleted:
		if id != "s1" {
			t.Fatalf("DELETE hit for id %q, want s1", id)
		}
	case u := <-unknown:
		t.Fatalf("DeleteSession hit unserved route %s; want DELETE /api/sessions/s1", u)
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE /api/sessions/s1 never received")
	}
}

func TestStopGeneration_SendsStopFrameOverWS(t *testing.T) {
	srv, _, frames, unknown := newSessionRoutesStandIn(t)
	client := NewRelayClient(srv.URL, "", "")

	client.StopGeneration("s1")

	select {
	case f := <-frames:
		if f["type"] != "stop_generation" || f["sessionId"] != "s1" {
			t.Fatalf("frame = %v, want stop_generation for s1", f)
		}
	case u := <-unknown:
		t.Fatalf("StopGeneration hit unserved route %s; want stop_generation frame on /ws", u)
	case <-time.After(5 * time.Second):
		t.Fatal("no stop_generation frame received")
	}
}
