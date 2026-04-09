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

func TestExtractAndStoreFindings(t *testing.T) {
	setupTestServer(t)

	// Valid repro_details with files_investigated and per-file references in summary
	reproDetails := `{
		"files_investigated": ["middleware.go", "handler.go"],
		"summary": "The streaming middleware.go buffers responses incorrectly. The handler.go routes requests to the middleware"
	}`
	extractAndStoreFindings("test/repo", 42, "BUG_CONFIRMED", "HIGH", reproDetails)

	findings, err := database.GetFindingsForRepo("test/repo", 10)
	if err != nil {
		t.Fatalf("GetFindingsForRepo: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}

	// Build a map for easier assertion
	byFile := map[string]string{}
	for _, f := range findings {
		byFile[f.FilePath] = f.Finding
		if f.Verdict != "BUG_CONFIRMED" {
			t.Errorf("verdict = %q, want BUG_CONFIRMED", f.Verdict)
		}
	}

	// Each file should have gotten its own per-file finding
	if mw, ok := byFile["middleware.go"]; !ok {
		t.Error("missing finding for middleware.go")
	} else if !strings.Contains(mw, "middleware.go buffers responses") {
		t.Errorf("middleware finding should reference buffering, got: %q", mw)
	}
	if h, ok := byFile["handler.go"]; !ok {
		t.Error("missing finding for handler.go")
	} else if !strings.Contains(h, "handler.go routes requests") {
		t.Errorf("handler finding should reference routing, got: %q", h)
	}
}

func TestExtractPerFileFindings(t *testing.T) {
	// Test the per-file extraction logic directly
	files := []any{"src/server.go", "config.go"}
	summary := "The bug is in server.go where Flush() is never called. The config.go file loads defaults incorrectly"

	result := extractPerFileFindings(summary, files)

	if f, ok := result["src/server.go"]; !ok {
		t.Error("expected finding for src/server.go via basename match")
	} else if !strings.Contains(f, "Flush") {
		t.Errorf("server finding should mention Flush, got: %q", f)
	}

	if f, ok := result["config.go"]; !ok {
		t.Error("expected finding for config.go")
	} else if !strings.Contains(f, "defaults") {
		t.Errorf("config finding should mention defaults, got: %q", f)
	}
}

func TestExtractAndStoreFindingsEmptyDetails(t *testing.T) {
	setupTestServer(t)

	// Should not panic with empty details
	extractAndStoreFindings("test/repo", 42, "BUG_CONFIRMED", "HIGH", "")
	extractAndStoreFindings("test/repo", 42, "BUG_CONFIRMED", "HIGH", "{}")
	extractAndStoreFindings("test/repo", 42, "BUG_CONFIRMED", "HIGH", `{"files_investigated": []}`)

	findings, _ := database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty details, got %d", len(findings))
	}
}

func TestExtractAndStoreFindingsNoSummary(t *testing.T) {
	setupTestServer(t)

	// When no summary, should use verdict as finding
	reproDetails := `{"files_investigated": ["main.go"]}`
	extractAndStoreFindings("test/repo", 10, "NOT_REPRODUCIBLE", "MEDIUM", reproDetails)

	findings, _ := database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Finding != "NOT_REPRODUCIBLE" {
		t.Errorf("finding = %q, want 'NOT_REPRODUCIBLE' (fallback to verdict)", findings[0].Finding)
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
