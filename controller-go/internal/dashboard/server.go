package dashboard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

//go:embed static/*
var staticFS embed.FS

// State holds shared controller state visible to the dashboard.
type State struct {
	mu          sync.RWMutex
	StartTime   time.Time
	LastPoll    string
	PollCount   int
	Repos       map[string]RepoStatus
	CheckNow    chan struct{}
	CheckResult *CheckResult
}

type RepoStatus struct {
	Pending    int    `json:"pending"`
	Processed  int    `json:"processed"`
	ManualOnly bool   `json:"manual_only"`
	LastCheck  string `json:"last_check"`
}

type CheckResult struct {
	Total int `json:"total"`
}

func NewState() *State {
	return &State{
		StartTime: time.Now(),
		Repos:     make(map[string]RepoStatus),
		CheckNow:  make(chan struct{}, 1),
	}
}

func (s *State) GetPollCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.PollCount
}

func (s *State) GetLastPoll() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastPoll
}

func (s *State) GetRepos() map[string]RepoStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]RepoStatus, len(s.Repos))
	for k, v := range s.Repos {
		result[k] = v
	}
	return result
}

func (s *State) UpdateRepo(repo string, status RepoStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Repos[repo] = status
}

func (s *State) TriggerCheckNow() {
	select {
	case s.CheckNow <- struct{}{}:
	default:
	}
}

func (s *State) SetPollInfo(count int, lastPoll string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PollCount = count
	s.LastPoll = lastPoll
}

func (s *State) SetCheckResult(result *CheckResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CheckResult = result
}

func (s *State) DeleteRepo(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Repos, repo)
}

// LogBuffer captures recent log lines for the admin page.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func NewLogBuffer(max int) *LogBuffer {
	return &LogBuffer{max: max, lines: make([]string, 0, max)}
}

func (lb *LogBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	line := strings.TrimRight(string(p), "\n")
	lb.lines = append(lb.lines, line)
	if len(lb.lines) > lb.max {
		lb.lines = lb.lines[len(lb.lines)-lb.max:]
	}
	return len(p), nil
}

func (lb *LogBuffer) Last(n int) []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if n > len(lb.lines) {
		n = len(lb.lines)
	}
	result := make([]string, n)
	copy(result, lb.lines[len(lb.lines)-n:])
	return result
}

// ReproduceFunc is the callback for creating reproduction jobs.
type ReproduceFunc func(repo string, issue int) error

// VerifyFixFunc is the callback for creating verify-fix jobs (with OPINAI_VERIFY_FIX=true).
type VerifyFixFunc func(repo string, issue int) error

// RerunFunc is the callback for re-running an issue (clears old state and creates a new job).
type RerunFunc func(repo string, issue int) error

// ClearRecordedFunc is a callback to clear the harvester's recorded map for an issue.
type ClearRecordedFunc func(repo string, issue int)

// MarkRecordedFunc marks a job as recorded so the harvester skips it.
type MarkRecordedFunc func(repo string, issue int)

// RetryPendingFunc triggers a check for pending issues in a repo after a job completes.
type RetryPendingFunc func(repo string)

// JobInfo describes an active K8s reproduction job.
type JobInfo struct {
	Repo      string `json:"repo"`
	Issue     int    `json:"issue"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	PodName   string `json:"pod_name"`
}

// ListJobsFunc returns active reproduction jobs.
type ListJobsFunc func() []JobInfo

// SandboxManagerIface abstracts sandbox operations for the dashboard.
type SandboxManagerIface interface {
	ListSandboxes() []SandboxInfo
	TeardownSandbox(ns string) bool
	AutoCleanup(maxAge int) int
}

// SandboxInfo matches sandbox.SandboxInfo.
type SandboxInfo struct {
	Namespace  string `json:"namespace"`
	Repo       string `json:"repo"`
	Issue      string `json:"issue"`
	CreatedAt  string `json:"created_at"`
	AgeSeconds int    `json:"age_seconds"`
}

// Server is the OpinAI dashboard HTTP/HTTPS server.
type Server struct {
	state     *State
	router    chi.Router
	logBuf    *LogBuffer
	hub       *Hub
	reproduce ReproduceFunc
	verifyFix VerifyFixFunc
	rerun         RerunFunc
	clearRecorded ClearRecordedFunc
	markRecorded  MarkRecordedFunc
	retryPending  RetryPendingFunc
	listJobs      ListJobsFunc
	sandbox       SandboxManagerIface
}

// New creates the dashboard server with all routes.
func New(state *State, logBuf *LogBuffer) *Server {
	s := &Server{state: state, logBuf: logBuf, hub: NewHub()}
	s.router = s.buildRouter()
	return s
}

// GetHub returns the WebSocket hub for broadcasting from external code.
func (s *Server) GetHub() *Hub { return s.hub }

// SetReproduceCallback sets the function called for /api/reproduce.
func (s *Server) SetReproduceCallback(fn ReproduceFunc) {
	s.reproduce = fn
}

// SetVerifyFixCallback sets the function called for /api/verify-fix.
func (s *Server) SetVerifyFixCallback(fn VerifyFixFunc) {
	s.verifyFix = fn
}

// SetRerunCallback sets the function called for /api/rerun.
func (s *Server) SetRerunCallback(fn RerunFunc) {
	s.rerun = fn
}

// SetClearRecordedCallback sets the function to clear harvester recorded state.
func (s *Server) SetClearRecordedCallback(fn ClearRecordedFunc) {
	s.clearRecorded = fn
}

// SetMarkRecordedCallback sets the function to mark a job as already recorded.
func (s *Server) SetMarkRecordedCallback(fn MarkRecordedFunc) {
	s.markRecorded = fn
}

// SetRetryPendingCallback sets the function to retry pending issues for a repo.
func (s *Server) SetRetryPendingCallback(fn RetryPendingFunc) {
	s.retryPending = fn
}

// SetListJobsCallback sets the function called for /api/jobs.
func (s *Server) SetListJobsCallback(fn ListJobsFunc) {
	s.listJobs = fn
}

// SetSandboxManager sets the sandbox manager for admin endpoints.
func (s *Server) SetSandboxManager(sm SandboxManagerIface) {
	s.sandbox = sm
}

func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(corsMiddleware)
	r.Use(rateLimitMiddleware)

	// Static files
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("failed to create static sub-fs", "error", err)
	} else {
		serveFile := func(name, ct string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				data, err := fs.ReadFile(staticSub, name)
				if err != nil {
					http.Error(w, "not found", 404)
					return
				}
				w.Header().Set("Content-Type", ct)
				// No caching for HTML (ensure fresh JS/CSS after deploys)
				if strings.HasSuffix(name, ".html") {
					w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				}
				w.Write(data)
			}
		}
		r.Get("/", serveFile("index.html", "text/html; charset=utf-8"))
		r.Get("/admin", serveFile("admin.html", "text/html; charset=utf-8"))
		r.Get("/style.css", serveFile("style.css", "text/css; charset=utf-8"))
	}

	// Health
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	// WebSocket
	r.Get("/ws", s.hub.HandleWS)

	// Public SSE endpoints (no auth)
	r.Post("/api/webhook/github", s.handleGitHubWebhook)

	// Protected SSE endpoints (bearer auth, outside jsonContentType)
	r.Group(func(r chi.Router) {
		r.Use(bearerAuthMiddleware)
		r.Get("/api/admin/analyze-stream", s.handleAnalyzeStream)
		r.Get("/api/reproduce-stream", s.handleReproduceStream)
		r.Post("/api/chat-stream", s.handleChatStream)
		r.Get("/api/check-now-stream", s.handleCheckNowStream)
		r.Get("/api/job-logs", s.handleJobLogs)
		r.Post("/api/internal/result", s.handleInternalResult)
	})

	// Core API
	r.Route("/api", func(r chi.Router) {
		r.Use(jsonContentType)

		// Public endpoints (no auth)
		r.Get("/status", s.handleStatus)
		r.Get("/repos", s.handleRepos)
		r.Get("/runs", s.handleRuns)
		r.Get("/jobs", s.handleJobs)
		r.Get("/report/{id}", s.handleReport)
		r.Get("/run-history", s.handleRunHistory)

		// Protected endpoints (bearer auth)
		r.Group(func(r chi.Router) {
			r.Use(bearerAuthMiddleware)
			r.Post("/check-now", s.handleCheckNow)
			r.Post("/reproduce", s.handleReproduce)
			r.Post("/verify-fix", s.handleVerifyFix)
			r.Post("/chat", s.handleChatFull)
			r.Get("/chat-history", s.handleChatHistory)
			r.Post("/chat-history/clear", s.handleClearChatHistory)
			r.Post("/runs/{id}/post-comment", s.handlePostComment)
			r.Delete("/runs/*", s.handleDeleteRuns)
			r.Post("/rerun/*", s.handleRerun)
			r.Post("/rerun-all/*", s.handleRerunAll)

			// Admin
			r.Route("/admin", func(r chi.Router) {
				r.Get("/repos", s.handleAdminReposGet)
				r.Post("/repos", s.handleAdminReposAdd)
				r.Put("/repos", s.handleAdminReposUpdate)
				r.Delete("/repos", s.handleAdminReposDelete)
				r.Get("/settings", s.handleAdminSettings)
				r.Put("/settings", s.handleAdminSettingsUpdate)
				r.Get("/system", s.handleAdminSystem)
				r.Get("/logs", s.handleAdminLogs)
				r.Post("/test-ai", s.handleAdminTestAI)
				r.Post("/test-github", s.handleAdminTestGitHub)
				r.Get("/db-stats", s.handleAdminDBStats)
				r.Get("/db-runs", s.handleAdminDBRuns)
				r.Get("/db-memory", s.handleAdminDBMemory)
				r.Get("/repo-memory/*", s.handleAdminRepoMemory)
				r.Get("/deployment-plan/*", s.handleAdminGetPlan)
				r.Put("/deployment-plan/*", s.handleAdminUpdatePlan)
				r.Post("/analyze-deployment", s.handleAdminAnalyze)
				r.Get("/sandboxes", s.handleAdminSandboxes)
				r.Delete("/sandboxes/*", s.handleAdminSandboxDelete)
				r.Post("/sandboxes/cleanup", s.handleAdminSandboxCleanup)
			})
		})
	})

	return r
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// NewHTTPServer creates an HTTP server (does not start it).
func (s *Server) NewHTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.router}
}

// NewHTTPSServer creates an HTTPS server with a self-signed cert (does not start it).
func (s *Server) NewHTTPSServer(addr string) *http.Server {
	tlsCfg, err := selfSignedTLSConfig()
	if err != nil {
		slog.Error("failed to generate TLS cert", "error", err)
		return nil
	}
	return &http.Server{Addr: addr, Handler: s.router, TLSConfig: tlsCfg}
}

// StartHTTP starts the HTTP server (blocks).
func (s *Server) StartHTTP(addr string) {
	slog.Info("dashboard HTTP server starting", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: s.router}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
	}
}

// StartHTTPS starts the HTTPS server with a self-signed certificate (blocks).
func (s *Server) StartHTTPS(addr string) {
	tlsCfg, err := selfSignedTLSConfig()
	if err != nil {
		slog.Error("failed to generate TLS cert", "error", err)
		return
	}
	slog.Info("dashboard HTTPS server starting", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: s.router, TLSConfig: tlsCfg}
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTPS server error", "error", err)
	}
}

func selfSignedTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "opinai-controller"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"opinai-controller", "localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// Env reads an env var with a default.
func Env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ParseRepos splits the REPOS env var.
func ParseRepos(s string) []string {
	var repos []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			repos = append(repos, r)
		}
	}
	return repos
}

// FormatDuration formats seconds into a human-readable string.
func FormatDuration(seconds float64) string {
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh %dm", s/3600, (s%3600)/60)
}
