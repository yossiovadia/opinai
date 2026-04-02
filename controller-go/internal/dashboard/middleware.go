package dashboard

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- Request Logging ---

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		dur := time.Since(start)
		// Skip noisy paths
		if r.URL.Path == "/healthz" || r.URL.Path == "/health" || r.URL.Path == "/ws" {
			return
		}
		slog.Info("HTTP",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ms", dur.Milliseconds(),
			"ip", r.RemoteAddr,
		)
	})
}

// --- CORS ---

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Rate Limiting ---

type rateBucket struct {
	tokens    float64
	lastFill  time.Time
	rate      float64 // tokens per second
	capacity  float64
}

func (b *rateBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.lastFill = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type rateLimiterStore struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

var defaultLimiter = &rateLimiterStore{buckets: make(map[string]*rateBucket)}
var aiLimiter = &rateLimiterStore{buckets: make(map[string]*rateBucket)}

func init() {
	// Cleanup stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			cleanBuckets(defaultLimiter)
			cleanBuckets(aiLimiter)
		}
	}()
}

func cleanBuckets(store *rateLimiterStore) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for ip, b := range store.buckets {
		if b.lastFill.Before(cutoff) {
			delete(store.buckets, ip)
		}
	}
}

func (s *rateLimiterStore) getBucket(ip string, rate, capacity float64) *rateBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[ip]
	if !ok {
		b = &rateBucket{tokens: capacity, lastFill: time.Now(), rate: rate, capacity: capacity}
		s.buckets[ip] = b
	}
	return b
}

// AI paths get stricter rate limiting
var aiPaths = []string{"/api/chat", "/api/chat-stream", "/api/admin/analyze", "/api/reproduce", "/api/verify-fix"}

func isAIPath(path string) bool {
	for _, p := range aiPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = strings.Split(fwd, ",")[0]
		}

		var allowed bool
		if isAIPath(r.URL.Path) {
			// 30 requests/minute = 0.5/sec, burst 5
			b := aiLimiter.getBucket(ip, 0.5, 5)
			allowed = b.allow()
		} else {
			// 120 requests/minute = 2/sec, burst 20
			b := defaultLimiter.getBucket(ip, 2.0, 20)
			allowed = b.allow()
		}

		if !allowed {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":true,"message":"Rate limit exceeded","status":429}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
