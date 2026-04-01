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
	CheckNow    bool
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

// Server is the OpinAI dashboard HTTP/HTTPS server.
type Server struct {
	state  *State
	router chi.Router
}

// New creates the dashboard server with all routes.
func New(state *State) *Server {
	s := &Server{state: state}
	s.router = s.buildRouter()
	return s
}

func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	// Static files
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("failed to create static sub-fs", "error", err)
	} else {
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			data, err := fs.ReadFile(staticSub, "index.html")
			if err != nil {
				http.Error(w, "not found", 404)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
		})
		r.Get("/admin", func(w http.ResponseWriter, req *http.Request) {
			data, err := fs.ReadFile(staticSub, "admin.html")
			if err != nil {
				http.Error(w, "not found", 404)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
		})
		r.Get("/style.css", func(w http.ResponseWriter, req *http.Request) {
			data, err := fs.ReadFile(staticSub, "style.css")
			if err != nil {
				http.Error(w, "not found", 404)
				return
			}
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Write(data)
		})
	}

	// Health
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// API
	r.Route("/api", func(r chi.Router) {
		r.Use(jsonContentType)
		r.Get("/status", s.handleStatus)
		r.Get("/repos", s.handleRepos)
		r.Get("/runs", s.handleRuns)
	})

	return r
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

// StartHTTP starts the HTTP server on the given address.
func (s *Server) StartHTTP(addr string) {
	slog.Info("dashboard HTTP server starting", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: s.router}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
	}
}

// StartHTTPS starts the HTTPS server with a self-signed certificate.
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
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
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
