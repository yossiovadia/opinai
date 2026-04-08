// Package infra manages infrastructure dependencies (databases, caches) for projects.
// Dependencies are installed into a shared "opinai-infra" namespace and persist across runs.
package infra

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

const InfraNamespace = "opinai-infra"

// Provider knows how to install, start, stop, and teardown a specific dependency type.
type Provider interface {
	// Install creates the K8s resources (StatefulSet/Deployment, Service, PVC) for this dep.
	Install(ctx context.Context, client kubernetes.Interface, namespace string) (connectionInfo string, err error)
	// Start scales the workload back up (from 0 replicas).
	Start(ctx context.Context, client kubernetes.Interface, namespace string) error
	// Stop scales the workload to 0 replicas (preserves PVC/data).
	Stop(ctx context.Context, client kubernetes.Interface, namespace string) error
	// Teardown deletes all K8s resources including PVCs.
	Teardown(ctx context.Context, client kubernetes.Interface, namespace string) error
	// IsRunning checks if the workload has ready pods.
	IsRunning(ctx context.Context, client kubernetes.Interface, namespace string) (bool, error)
	// ConnectionInfo returns the connection string for a running dep.
	ConnectionInfo(namespace string) string
	// EnvVarName returns the env var name used to pass connection info to runners.
	EnvVarName() string
}

// Manager orchestrates infrastructure dependency lifecycle.
type Manager struct {
	client    kubernetes.Interface
	mu        sync.Mutex
	providers map[string]Provider
}

// NewManager creates an InfraManager with the built-in providers.
func NewManager(client kubernetes.Interface) *Manager {
	m := &Manager{
		client: client,
		providers: map[string]Provider{
			"postgresql": &PostgreSQLProvider{},
			"redis":      &RedisProvider{},
			"mongodb":    &MongoDBProvider{},
		},
	}
	return m
}

// unsupportedDeps are infra_deps that require operators or complex setup.
// We log a clear message instead of attempting installation.
var unsupportedDeps = map[string]string{
	"kuadrant":   "Kuadrant operator not available — code review only for Kuadrant-specific features",
	"kserve":     "KServe operator not available — code review only for KServe-specific features",
	"kubernetes": "Generic Kubernetes requirement — sandbox deployment handles this",
	"docker":     "Docker-in-Docker not supported in cluster — code review only for Docker-specific features",
	"istio":      "Istio service mesh not available — code review only for Istio-specific features",
}

// EnsureRunning ensures a dependency is installed and running, returning connection info.
// This is the main entry point called before creating runner Jobs.
func (m *Manager) EnsureRunning(dep string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if this is an unsupported dep
	if msg, ok := unsupportedDeps[dep]; ok {
		slog.Info("infra dependency not supported", "dep", dep, "reason", msg)
		return "", fmt.Errorf("%s", msg)
	}

	provider, ok := m.providers[dep]
	if !ok {
		return "", fmt.Errorf("no provider for infrastructure dependency %q", dep)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Ensure the infra namespace exists
	if err := m.ensureNamespace(ctx); err != nil {
		return "", fmt.Errorf("ensure infra namespace: %w", err)
	}

	// Check current state in DB
	existing, err := database.GetInfraDep(dep)
	if err != nil {
		return "", fmt.Errorf("get infra dep %s: %w", dep, err)
	}

	switch {
	case existing == nil || existing.Status == "not_installed":
		// Fresh install
		slog.Info("installing infrastructure dependency", "dep", dep)
		connInfo, installErr := provider.Install(ctx, m.client, InfraNamespace)
		if installErr != nil {
			m.saveStatus(dep, "failed")
			return "", fmt.Errorf("install %s: %w", dep, installErr)
		}
		now := database.Now()
		if err := database.UpsertInfraDep(database.InfraDep{
			Name:        dep,
			Namespace:   InfraNamespace,
			Status:      "running",
			InstalledAt: &now,
			LastUsedAt:  &now,
			Connection:  connInfo,
		}); err != nil {
			return "", fmt.Errorf("save infra dep %s: %w", dep, err)
		}
		slog.Info("infrastructure dependency installed", "dep", dep, "connection", connInfo)
		return connInfo, nil

	case existing.Status == "running":
		// Verify it's actually running
		running, checkErr := provider.IsRunning(ctx, m.client, InfraNamespace)
		if checkErr != nil || !running {
			slog.Warn("infra dep marked running but not healthy, restarting", "dep", dep)
			if startErr := provider.Start(ctx, m.client, InfraNamespace); startErr != nil {
				m.saveStatus(dep, "failed")
				return "", fmt.Errorf("restart %s: %w", dep, startErr)
			}
		}
		database.TouchInfraDepUsed(dep)
		return existing.Connection, nil

	case existing.Status == "stopped":
		slog.Info("starting stopped infrastructure dependency", "dep", dep)
		if startErr := provider.Start(ctx, m.client, InfraNamespace); startErr != nil {
			m.saveStatus(dep, "failed")
			return "", fmt.Errorf("start %s: %w", dep, startErr)
		}
		m.saveStatus(dep, "running")
		database.TouchInfraDepUsed(dep)
		return existing.Connection, nil

	case existing.Status == "failed":
		slog.Info("reinstalling failed infrastructure dependency", "dep", dep)
		// Try teardown first to clean up partial state
		_ = provider.Teardown(ctx, m.client, InfraNamespace)
		connInfo, installErr := provider.Install(ctx, m.client, InfraNamespace)
		if installErr != nil {
			m.saveStatus(dep, "failed")
			return "", fmt.Errorf("reinstall %s: %w", dep, installErr)
		}
		now := database.Now()
		if err := database.UpsertInfraDep(database.InfraDep{
			Name:        dep,
			Namespace:   InfraNamespace,
			Status:      "running",
			InstalledAt: &now,
			LastUsedAt:  &now,
			Connection:  connInfo,
		}); err != nil {
			return "", fmt.Errorf("save infra dep %s: %w", dep, err)
		}
		return connInfo, nil

	case existing.Status == "installing":
		// Another goroutine may be installing — check if it's actually running now
		running, _ := provider.IsRunning(ctx, m.client, InfraNamespace)
		if running {
			m.saveStatus(dep, "running")
			database.TouchInfraDepUsed(dep)
			connInfo := provider.ConnectionInfo(InfraNamespace)
			return connInfo, nil
		}
		return "", fmt.Errorf("%s is currently being installed by another process", dep)

	default:
		return "", fmt.Errorf("unknown status %q for dep %s", existing.Status, dep)
	}
}

// Stop scales a dependency to 0 replicas, preserving data.
func (m *Manager) Stop(dep string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	provider, ok := m.providers[dep]
	if !ok {
		return fmt.Errorf("no provider for %q", dep)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := provider.Stop(ctx, m.client, InfraNamespace); err != nil {
		return fmt.Errorf("stop %s: %w", dep, err)
	}
	m.saveStatus(dep, "stopped")
	slog.Info("infrastructure dependency stopped", "dep", dep)
	return nil
}

// Teardown fully removes a dependency including PVCs.
func (m *Manager) Teardown(dep string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	provider, ok := m.providers[dep]
	if !ok {
		return fmt.Errorf("no provider for %q", dep)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := provider.Teardown(ctx, m.client, InfraNamespace); err != nil {
		return fmt.Errorf("teardown %s: %w", dep, err)
	}
	if err := database.DeleteInfraDep(dep); err != nil {
		return fmt.Errorf("delete infra dep record %s: %w", dep, err)
	}
	slog.Info("infrastructure dependency torn down", "dep", dep)
	return nil
}

// Status returns the current state of a dependency.
func (m *Manager) Status(dep string) (*database.InfraDep, error) {
	return database.GetInfraDep(dep)
}

// ListAll returns all managed infrastructure dependencies.
func (m *Manager) ListAll() ([]database.InfraDep, error) {
	return database.ListInfraDeps()
}

// EnsureAllRunning ensures all specified deps are running, collecting connection info.
// Returns a map of dep name -> connection string. Errors are logged but don't block other deps.
func (m *Manager) EnsureAllRunning(deps []string) map[string]string {
	result := make(map[string]string)
	for _, dep := range deps {
		connInfo, err := m.EnsureRunning(dep)
		if err != nil {
			slog.Warn("failed to ensure infra dependency", "dep", dep, "error", err)
			continue
		}
		result[dep] = connInfo
	}
	return result
}

func (m *Manager) ensureNamespace(ctx context.Context) error {
	if m.client == nil {
		return fmt.Errorf("K8s client not available")
	}
	_, err := m.client.CoreV1().Namespaces().Get(ctx, InfraNamespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: InfraNamespace,
			Labels: map[string]string{
				"opinai.dev/managed": "true",
				"opinai.dev/role":    "infrastructure",
			},
		},
	}
	_, err = m.client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	slog.Info("created infrastructure namespace", "namespace", InfraNamespace)
	return nil
}

func (m *Manager) saveStatus(dep, status string) {
	if err := database.UpdateInfraDepStatus(dep, status); err != nil {
		slog.Warn("failed to update infra dep status", "dep", dep, "status", status, "error", err)
	}
}
