package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

func setupTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	if err := database.Init(dbPath); err != nil {
		t.Fatalf("DB init: %v", err)
	}
	state := NewState()
	state.UpdateRepo("test/repo", RepoStatus{Pending: 3, Processed: 5})
	logBuf := NewLogBuffer(50)
	srv := New(state, logBuf)
	return srv
}

func TestHealthEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", w.Body.String())
	}
}

func TestStatusEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var data map[string]any
	json.Unmarshal(w.Body.Bytes(), &data)
	if _, ok := data["uptime_seconds"]; !ok {
		t.Error("missing uptime_seconds")
	}
	if _, ok := data["repos_count"]; !ok {
		t.Error("missing repos_count")
	}
	if data["repos_count"].(float64) != 1 {
		t.Errorf("repos_count = %v, want 1", data["repos_count"])
	}
}

func TestReposEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequest("GET", "/api/repos", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var repos []map[string]any
	json.Unmarshal(w.Body.Bytes(), &repos)
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(repos))
	}
	if repos[0]["name"] != "test/repo" {
		t.Errorf("name = %v", repos[0]["name"])
	}
}

func TestRunsEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	database.AddRun(database.Run{Repo: "test/repo", Issue: 1, Verdict: "BUG_CONFIRMED", CreatedAt: "2026-01-01"})

	req := httptest.NewRequest("GET", "/api/runs", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var runs []map[string]any
	json.Unmarshal(w.Body.Bytes(), &runs)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
}

func TestReproduceValidation(t *testing.T) {
	srv := setupTestServer(t)
	os.Setenv("REPOS", "test/repo")
	defer os.Unsetenv("REPOS")

	// Unknown repo
	body := `{"repo":"unknown/repo","issue_number":1}`
	req := httptest.NewRequest("POST", "/api/reproduce", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("unknown repo: status = %d, want 403", w.Code)
	}

	// Missing fields
	req2 := httptest.NewRequest("POST", "/api/reproduce", strings.NewReader(`{}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.router.ServeHTTP(w2, req2)
	if w2.Code != 400 {
		t.Errorf("missing fields: status = %d, want 400", w2.Code)
	}
}

func TestJsonErrorFormat(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, "test error", 422)
	if w.Code != 422 {
		t.Errorf("status = %d, want 422", w.Code)
	}
	var data map[string]any
	json.Unmarshal(w.Body.Bytes(), &data)
	if data["error"] != true {
		t.Errorf("error = %v, want true", data["error"])
	}
	if data["message"] != "test error" {
		t.Errorf("message = %v", data["message"])
	}
	if data["status"].(float64) != 422 {
		t.Errorf("status field = %v", data["status"])
	}
}
