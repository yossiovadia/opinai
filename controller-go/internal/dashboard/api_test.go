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

func TestBackfillFindings(t *testing.T) {
	setupTestServer(t)

	// Add runs with repro_details
	database.AddRun(database.Run{
		Repo: "test/repo", Issue: 1, Verdict: "BUG_CONFIRMED", Confidence: "HIGH",
		ReproDetails: `{"files_investigated":["main.go","server.go"],"summary":"main.go has a nil pointer. server.go handles connections"}`,
		CreatedAt:    "2026-01-01T00:00:00Z",
	})
	database.AddRun(database.Run{
		Repo: "test/repo", Issue: 2, Verdict: "NOT_REPRODUCIBLE", Confidence: "MEDIUM",
		ReproDetails: `{"files_investigated":["handler.go"],"summary":"handler.go looks fine"}`,
		CreatedAt:    "2026-01-02T00:00:00Z",
	})
	// Run without repro_details — should be skipped
	database.AddRun(database.Run{
		Repo: "test/repo", Issue: 3, Verdict: "BUG_CONFIRMED", Confidence: "HIGH",
		CreatedAt: "2026-01-03T00:00:00Z",
	})
	// Run without verdict — should be skipped
	database.AddRun(database.Run{
		Repo: "test/repo", Issue: 4,
		ReproDetails: `{"files_investigated":["other.go"]}`,
		CreatedAt:    "2026-01-04T00:00:00Z",
	})

	// Run backfill
	BackfillFindings()

	// Check findings were created for issues 1 and 2
	findings, _ := database.GetFindingsForRepo("test/repo", 50)
	if len(findings) != 3 { // 2 files from issue 1, 1 from issue 2
		t.Fatalf("expected 3 findings, got %d", len(findings))
	}

	// Run backfill again — should be idempotent (no duplicates)
	BackfillFindings()
	findings2, _ := database.GetFindingsForRepo("test/repo", 50)
	if len(findings2) != 3 {
		t.Errorf("expected 3 findings after second backfill (idempotent), got %d", len(findings2))
	}
}

func TestBackfillFindingsEmpty(t *testing.T) {
	setupTestServer(t)

	// No runs — should not error
	BackfillFindings()

	findings, _ := database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on empty DB, got %d", len(findings))
	}
}

func TestExtractFilePathsFromText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected []string
	}{
		{
			name:     "backtick paths",
			text:     "The issue is in `src/main.go` and also affects `handler.go`",
			expected: []string{"src/main.go", "handler.go"},
		},
		{
			name:     "paths with line numbers",
			text:     "See `middleware.go:42` for the problem",
			expected: []string{"middleware.go"},
		},
		{
			name:     "no file paths",
			text:     "This is a general comment with no file references",
			expected: nil,
		},
		{
			name:     "deduplication",
			text:     "Both `main.go` and `main.go` are referenced",
			expected: []string{"main.go"},
		},
		{
			name:     "various extensions",
			text:     "Check `app.py`, `config.yaml`, and `test.js`",
			expected: []string{"app.py", "config.yaml", "test.js"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractFilePathsFromText(tc.text)
			if len(result) != len(tc.expected) {
				t.Fatalf("expected %d paths, got %d: %v", len(tc.expected), len(result), result)
			}
			for i, exp := range tc.expected {
				if result[i] != exp {
					t.Errorf("path[%d] = %q, want %q", i, result[i], exp)
				}
			}
		})
	}
}

func TestExtractAndStoreFindingsFromPRReview(t *testing.T) {
	setupTestServer(t)

	reviewText := "The `middleware.go` has a buffering issue where Flush() is never called. " +
		"Also `handler.go` should validate input before processing."

	extractAndStoreFindingsFromPRReview("test/repo", 100, "CHANGES_REQUESTED", reviewText)

	findings, _ := database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings from PR review, got %d", len(findings))
	}

	// Check that verdict has PR_REVIEW prefix
	for _, f := range findings {
		if f.Verdict != "PR_REVIEW:CHANGES_REQUESTED" {
			t.Errorf("verdict = %q, want PR_REVIEW:CHANGES_REQUESTED", f.Verdict)
		}
		if f.IssueNumber != 100 {
			t.Errorf("issue_number = %d, want 100 (PR number)", f.IssueNumber)
		}
	}
}

func TestExtractAndStoreFindingsFromPRReviewEmpty(t *testing.T) {
	setupTestServer(t)

	// No file references — should store nothing
	extractAndStoreFindingsFromPRReview("test/repo", 50, "APPROVE", "LGTM, looks good!")
	findings, _ := database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for review without file references, got %d", len(findings))
	}

	// Empty text — should store nothing
	extractAndStoreFindingsFromPRReview("test/repo", 51, "COMMENT", "")
	findings, _ = database.GetFindingsForRepo("test/repo", 10)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty review, got %d", len(findings))
	}
}

func TestLooksLikeFilePath(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"main.go", true},
		{"src/handler.py", true},
		{"config.yaml", true},
		{"noextension", false},
		{"", false},
		{"has spaces.go", false},
		{".go", false},
		{"very/deep/nested/path/to/file.ts", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if result := looksLikeFilePath(tc.input); result != tc.expected {
				t.Errorf("looksLikeFilePath(%q) = %v, want %v", tc.input, result, tc.expected)
			}
		})
	}
}

func TestCheckOutcomesEndpoint(t *testing.T) {
	srv := setupTestServer(t)

	// Without callback — should return 503
	req := httptest.NewRequest("POST", "/api/admin/check-outcomes", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Errorf("without callback: status = %d, want 503", w.Code)
	}

	// With callback — should return 200
	srv.SetCheckOutcomesCallback(func() {})
	req2 := httptest.NewRequest("POST", "/api/admin/check-outcomes", nil)
	w2 := httptest.NewRecorder()
	srv.router.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Errorf("with callback: status = %d, want 200", w2.Code)
	}
	// Note: callback runs in goroutine, so 'called' may not be true yet
	// but we verify the endpoint responds correctly
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
