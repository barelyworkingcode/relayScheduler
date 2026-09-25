package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const chatTaskJSON = `{"name":"digest","projectId":"p1","prompt":"summarize","model":"haiku","enabled":true,"schedule":{"type":"interval","minutes":15}}`

func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store := NewTaskStore(dir)
	logStore := NewLogStore(filepath.Join(dir, "task-logs"))
	mux := http.NewServeMux()
	RegisterRoutes(mux, store, NewScheduler(nil, store, logStore, NewHub(store)), logStore)
	return mux
}

func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func createTask(t *testing.T, h http.Handler, body string) Task {
	t.Helper()
	rec := serve(h, http.MethodPost, "/api/tasks", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/tasks = %d %s, want 201", rec.Code, rec.Body)
	}
	var task Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	return task
}

func TestAPI_TaskLifecycle(t *testing.T) {
	h := newTestAPI(t)
	created := createTask(t, h, chatTaskJSON)
	if created.ID == "" || created.CreatedAt == "" {
		t.Fatalf("created task missing id/createdAt: %+v", created)
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/tasks", http.StatusOK},
		{http.MethodGet, "/api/tasks/" + created.ID, http.StatusOK},
		{http.MethodGet, "/api/tasks/" + created.ID + "/history", http.StatusOK},
		{http.MethodGet, "/api/tasks/missing", http.StatusNotFound},
		{http.MethodGet, "/api/tasks/missing/history", http.StatusNotFound},
		{http.MethodPost, "/api/tasks/missing/run", http.StatusNotFound},
		{http.MethodPatch, "/api/tasks/" + created.ID, http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/tasks/" + created.ID + "/run", http.StatusMethodNotAllowed},
	} {
		if rec := serve(h, tc.method, tc.path, ""); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}

	if rec := serve(h, http.MethodGet, "/api/tasks?projectId=other", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("GET ?projectId=other = %s, want []", rec.Body)
	}

	rec := serve(h, http.MethodPut, "/api/tasks/"+created.ID, strings.Replace(chatTaskJSON, "digest", "renamed", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", rec.Code, rec.Body)
	}
	var updated Task
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated task: %v", err)
	}
	if updated.Name != "renamed" || updated.CreatedAt != created.CreatedAt {
		t.Errorf("updated = %+v, want name renamed and createdAt preserved", updated)
	}

	if rec := serve(h, http.MethodDelete, "/api/tasks/"+created.ID, ""); rec.Code != http.StatusOK {
		t.Errorf("DELETE = %d, want 200", rec.Code)
	}
	if rec := serve(h, http.MethodGet, "/api/tasks/"+created.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}
}

func TestAPI_RejectsInvalidTasksOnCreateAndUpdate(t *testing.T) {
	h := newTestAPI(t)
	created := createTask(t, h, chatTaskJSON)

	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{`},
		{"missing schedule", `{"name":"digest","projectId":"p1","prompt":"summarize"}`},
		{"unknown schedule type", `{"name":"digest","projectId":"p1","prompt":"summarize","schedule":{"type":"bogus"}}`},
		{"chat task without prompt", `{"name":"digest","projectId":"p1","model":"haiku","schedule":{"type":"interval","minutes":15}}`},
		{"pty task without template", `{"name":"build","projectId":"p1","sessionType":"pty","schedule":{"type":"interval","minutes":15}}`},
		{"unknown session type", `{"name":"x","projectId":"p1","prompt":"p","sessionType":"voice","schedule":{"type":"interval","minutes":15}}`},
	} {
		if rec := serve(h, http.MethodPost, "/api/tasks", tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", tc.name, rec.Code)
		}
		if rec := serve(h, http.MethodPut, "/api/tasks/"+created.ID, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", tc.name, rec.Code)
		}
	}

	rec := serve(h, http.MethodGet, "/api/tasks/"+created.ID, "")
	var stored Task
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode stored task: %v", err)
	}
	if st, _ := ScheduleType(stored.Schedule); st != "interval" {
		t.Errorf("stored schedule type = %q after rejected updates, want interval", st)
	}
}

func TestAPI_ChatTaskRequiresModel(t *testing.T) {
	h := newTestAPI(t)
	created := createTask(t, h, chatTaskJSON)

	for _, tc := range []struct{ name, body string }{
		{"empty model", `{"name":"digest","projectId":"p1","prompt":"summarize","model":"","schedule":{"type":"interval","minutes":15}}`},
		{"whitespace model, headless", `{"name":"digest","projectId":"p1","prompt":"summarize","sessionType":"headless","model":"   ","schedule":{"type":"interval","minutes":15}}`},
	} {
		for _, req := range []struct{ method, path string }{
			{http.MethodPost, "/api/tasks"},
			{http.MethodPut, "/api/tasks/" + created.ID},
		} {
			rec := serve(h, req.method, req.path, tc.body)
			var body struct{ Error string }
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(body.Error, "digest") || !strings.Contains(body.Error, "model") {
				t.Errorf("%s %s = %d %s, want 400 with an error naming the task and model", req.method, tc.name, rec.Code, rec.Body)
			}
		}
	}

	createTask(t, h, `{"name":"build","projectId":"p1","sessionType":"pty","templateId":"shell","schedule":{"type":"interval","minutes":15}}`)
}

func TestAPI_DeleteByProject(t *testing.T) {
	h := newTestAPI(t)
	createTask(t, h, chatTaskJSON)
	createTask(t, h, chatTaskJSON)
	createTask(t, h, strings.Replace(chatTaskJSON, `"p1"`, `"p2"`, 1))

	rec := serve(h, http.MethodDelete, "/api/tasks/by-project/p1", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"deleted":2}` {
		t.Fatalf("DELETE by-project = %d %s, want 200 {\"deleted\":2}", rec.Code, rec.Body)
	}

	var remaining []Task
	if err := json.Unmarshal(serve(h, http.MethodGet, "/api/tasks", "").Body.Bytes(), &remaining); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ProjectID != "p2" {
		t.Errorf("remaining = %+v, want the single p2 task", remaining)
	}
}
