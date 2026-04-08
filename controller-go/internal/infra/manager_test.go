package infra

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := database.Init(path); err != nil {
		t.Fatalf("Init DB: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

func TestNewManagerHasProviders(t *testing.T) {
	m := NewManager(nil)
	for _, dep := range []string{"postgresql", "redis", "mongodb"} {
		if _, ok := m.providers[dep]; !ok {
			t.Errorf("missing provider for %q", dep)
		}
	}
}

func TestEnsureRunningUnsupportedDep(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	for _, dep := range []string{"kuadrant", "kserve", "istio", "docker"} {
		_, err := m.EnsureRunning(dep)
		if err == nil {
			t.Errorf("expected error for unsupported dep %q", dep)
		}
	}
}

func TestEnsureRunningUnknownProvider(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	_, err := m.EnsureRunning("cockroachdb")
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestEnsureAllRunningGracefulDegradation(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	// All unsupported deps should fail gracefully without blocking
	result := m.EnsureAllRunning([]string{"kuadrant", "kserve", "nonexistent"})
	if len(result) != 0 {
		t.Errorf("expected empty result for all-failing deps, got %v", result)
	}
}

func TestInfraDepDatabaseCRUD(t *testing.T) {
	setupTestDB(t)

	// Initially empty
	deps, err := database.ListInfraDeps()
	if err != nil {
		t.Fatalf("ListInfraDeps: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}

	// Insert
	now := database.Now()
	err = database.UpsertInfraDep(database.InfraDep{
		Name:        "postgresql",
		Namespace:   "opinai-infra",
		Status:      "running",
		InstalledAt: &now,
		LastUsedAt:  &now,
		Connection:  "postgresql://opinai:opinai@opinai-postgresql.opinai-infra.svc:5432/opinai",
	})
	if err != nil {
		t.Fatalf("UpsertInfraDep: %v", err)
	}

	// Get by name
	dep, err := database.GetInfraDep("postgresql")
	if err != nil {
		t.Fatalf("GetInfraDep: %v", err)
	}
	if dep == nil {
		t.Fatal("expected non-nil dep")
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if dep.Connection != "postgresql://opinai:opinai@opinai-postgresql.opinai-infra.svc:5432/opinai" {
		t.Errorf("connection = %q", dep.Connection)
	}

	// Get non-existent
	dep2, err := database.GetInfraDep("redis")
	if err != nil {
		t.Fatalf("GetInfraDep(redis): %v", err)
	}
	if dep2 != nil {
		t.Error("expected nil for non-existent dep")
	}

	// Update status
	err = database.UpdateInfraDepStatus("postgresql", "stopped")
	if err != nil {
		t.Fatalf("UpdateInfraDepStatus: %v", err)
	}
	dep, _ = database.GetInfraDep("postgresql")
	if dep.Status != "stopped" {
		t.Errorf("status = %q, want stopped", dep.Status)
	}

	// Touch last_used_at
	err = database.TouchInfraDepUsed("postgresql")
	if err != nil {
		t.Fatalf("TouchInfraDepUsed: %v", err)
	}

	// Add another dep
	database.UpsertInfraDep(database.InfraDep{
		Name:      "redis",
		Namespace: "opinai-infra",
		Status:    "running",
	})

	// List all
	deps, err = database.ListInfraDeps()
	if err != nil {
		t.Fatalf("ListInfraDeps: %v", err)
	}
	if len(deps) != 2 {
		t.Errorf("expected 2 deps, got %d", len(deps))
	}

	// Delete
	err = database.DeleteInfraDep("postgresql")
	if err != nil {
		t.Fatalf("DeleteInfraDep: %v", err)
	}
	dep, _ = database.GetInfraDep("postgresql")
	if dep != nil {
		t.Error("expected nil after delete")
	}

	// Upsert existing (update)
	database.UpsertInfraDep(database.InfraDep{
		Name:       "redis",
		Namespace:  "opinai-infra",
		Status:     "failed",
		Connection: "redis://new-url",
	})
	dep, _ = database.GetInfraDep("redis")
	if dep.Status != "failed" {
		t.Errorf("status after upsert = %q, want failed", dep.Status)
	}
	if dep.Connection != "redis://new-url" {
		t.Errorf("connection after upsert = %q", dep.Connection)
	}
}

func TestProviderConnectionInfo(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		wantEnv  string
		wantConn string
	}{
		{
			name:     "postgresql",
			provider: &PostgreSQLProvider{},
			wantEnv:  "OPINAI_INFRA_POSTGRESQL",
			wantConn: "postgresql://opinai:opinai@opinai-postgresql.opinai-infra.svc:5432/opinai?sslmode=disable",
		},
		{
			name:     "redis",
			provider: &RedisProvider{},
			wantEnv:  "OPINAI_INFRA_REDIS",
			wantConn: "redis://opinai-redis.opinai-infra.svc:6379",
		},
		{
			name:     "mongodb",
			provider: &MongoDBProvider{},
			wantEnv:  "OPINAI_INFRA_MONGODB",
			wantConn: "mongodb://opinai:opinai@opinai-mongodb.opinai-infra.svc:27017/opinai?authSource=admin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.provider.EnvVarName(); got != tt.wantEnv {
				t.Errorf("EnvVarName() = %q, want %q", got, tt.wantEnv)
			}
			if got := tt.provider.ConnectionInfo(InfraNamespace); got != tt.wantConn {
				t.Errorf("ConnectionInfo() = %q, want %q", got, tt.wantConn)
			}
		})
	}
}

func TestUnsupportedDepsMessages(t *testing.T) {
	for dep, msg := range unsupportedDeps {
		if msg == "" {
			t.Errorf("empty message for unsupported dep %q", dep)
		}
		_ = dep
	}
}

func TestManagerStatusAndListWithNilClient(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	// Status for non-existent dep
	dep, err := m.Status("postgresql")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if dep != nil {
		t.Error("expected nil for non-existent dep")
	}

	// ListAll empty
	deps, err := m.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}

	// Insert one manually and verify listing works
	database.UpsertInfraDep(database.InfraDep{
		Name:      "postgresql",
		Namespace: "opinai-infra",
		Status:    "running",
	})
	deps, _ = m.ListAll()
	if len(deps) != 1 {
		t.Errorf("expected 1 dep, got %d", len(deps))
	}
	dep, _ = m.Status("postgresql")
	if dep == nil || dep.Status != "running" {
		t.Errorf("expected running dep, got %v", dep)
	}
}

func TestStopAndTeardownUnknownProvider(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	if err := m.Stop("unknown"); err == nil {
		t.Error("expected error for unknown provider stop")
	}
	if err := m.Teardown("unknown"); err == nil {
		t.Error("expected error for unknown provider teardown")
	}
}

func TestEnsureRunningNilClient(t *testing.T) {
	setupTestDB(t)
	m := NewManager(nil)

	// EnsureRunning with nil client should fail when trying to create namespace
	_, err := m.EnsureRunning("postgresql")
	if err == nil {
		t.Error("expected error with nil K8s client")
	}
}

