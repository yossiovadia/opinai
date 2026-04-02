package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSHeaders(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Errorf("CORS origin = %q", w.Header().Get("Access-Control-Allow-Origin"))
	}
	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing Allow-Methods")
	}
}

func TestCORSPreflight(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should not reach"))
	}))

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
}

func TestCORSSkipsWebSocket(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS should skip /ws")
	}
}

func TestRateLimiter(t *testing.T) {
	handler := rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	// AI endpoint: 5 burst, then 429
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/api/chat", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}

	// 6th should be rate limited
	req := httptest.NewRequest("GET", "/api/chat", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Errorf("6th request: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

func TestRateLimiterSkipsWebSocket(t *testing.T) {
	handler := rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Error("rate limiter should skip /ws")
	}
}

func TestLogBuffer(t *testing.T) {
	lb := NewLogBuffer(5)
	for i := 0; i < 10; i++ {
		lb.Write([]byte("line\n"))
	}
	lines := lb.Last(100)
	if len(lines) != 5 {
		t.Errorf("expected 5 lines (max), got %d", len(lines))
	}
	lines2 := lb.Last(3)
	if len(lines2) != 3 {
		t.Errorf("Last(3) = %d lines", len(lines2))
	}
}
