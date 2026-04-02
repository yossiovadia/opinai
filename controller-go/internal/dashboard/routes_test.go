package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

func TestRerunEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	// Pre-populate a processed issue
	database.MarkProcessed("owner/repo", 42, "old-job")

	req := httptest.NewRequest("POST", "/api/rerun/owner/repo/42", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var data map[string]any
	json.Unmarshal(w.Body.Bytes(), &data)
	if data["status"] != "rerun_triggered" {
		t.Errorf("status = %v", data["status"])
	}
	if data["repo"] != "owner/repo" {
		t.Errorf("repo = %v", data["repo"])
	}
}

func TestChatEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	body := `{"message":"hello","context":{}}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var data map[string]string
	json.Unmarshal(w.Body.Bytes(), &data)
	if _, ok := data["reply"]; !ok {
		t.Error("missing reply field")
	}
}

func TestChatHistoryEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	database.AddChatMessage("r/r", 1, "user", "hello")
	database.AddChatMessage("r/r", 1, "ai", "hi")
	req := httptest.NewRequest("GET", "/api/chat-history?repo=r/r&issue=1", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var msgs []map[string]any
	json.Unmarshal(w.Body.Bytes(), &msgs)
	if len(msgs) < 2 {
		t.Errorf("expected >=2 messages, got %d", len(msgs))
	}
}

func TestCheckNowEndpoint(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequest("POST", "/api/check-now", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var data map[string]string
	json.Unmarshal(w.Body.Bytes(), &data)
	if data["status"] != "triggered" {
		t.Errorf("status = %v", data["status"])
	}
}

func TestCSSServed(t *testing.T) {
	srv := setupTestServer(t)
	req := httptest.NewRequest("GET", "/style.css", nil)
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("content-type = %q", w.Header().Get("Content-Type"))
	}
}
