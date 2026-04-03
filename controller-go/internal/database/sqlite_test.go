package database

import (
	"os"
	"path/filepath"
	"testing"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	if err := Init(path); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

func TestInitDB(t *testing.T) {
	setupTestDB(t)
	// Verify tables exist
	for _, table := range []string{"runs", "repo_memory", "processed_issues", "deployment_plans", "chat_history"} {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}
}

func TestAddRunGetRuns(t *testing.T) {
	setupTestDB(t)
	id, err := AddRun(Run{
		Repo: "owner/repo", Issue: 42, Title: "test bug",
		Category: "BUG", Verdict: "BUG_CONFIRMED", Confidence: "HIGH",
		Report: "## Report", AIPowered: true, Duration: "5s",
		CreatedAt: "2026-04-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("AddRun: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	runs, err := GetRuns("owner/repo", 10)
	if err != nil {
		t.Fatalf("GetRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].Verdict != "BUG_CONFIRMED" {
		t.Errorf("verdict = %q, want BUG_CONFIRMED", runs[0].Verdict)
	}
	if runs[0].Issue != 42 {
		t.Errorf("issue = %d, want 42", runs[0].Issue)
	}

	// Filter by different repo
	runs2, _ := GetRuns("other/repo", 10)
	if len(runs2) != 0 {
		t.Errorf("expected 0 runs for other repo, got %d", len(runs2))
	}

	// No filter
	allRuns, _ := GetRuns("", 10)
	if len(allRuns) != 1 {
		t.Errorf("expected 1 total run, got %d", len(allRuns))
	}
}

func TestGetRun(t *testing.T) {
	setupTestDB(t)
	id, _ := AddRun(Run{Repo: "r/r", Issue: 1, CreatedAt: "2026-01-01"})
	run, err := GetRun(id)
	if err != nil || run == nil {
		t.Fatalf("GetRun(%d): %v", id, err)
	}
	if run.Repo != "r/r" {
		t.Errorf("repo = %q", run.Repo)
	}

	// Non-existent
	run2, _ := GetRun(9999)
	if run2 != nil {
		t.Error("expected nil for non-existent run")
	}
}

func TestMarkPosted(t *testing.T) {
	setupTestDB(t)
	id, _ := AddRun(Run{Repo: "r/r", Issue: 1, CreatedAt: "2026-01-01"})
	run, _ := GetRun(id)
	if run.Posted {
		t.Error("should not be posted initially")
	}
	MarkPosted(id)
	run, _ = GetRun(id)
	if !run.Posted {
		t.Error("should be posted after MarkPosted")
	}
}

func TestIsProcessedMarkProcessed(t *testing.T) {
	setupTestDB(t)
	ok, _ := IsProcessed("r/r", 1)
	if ok {
		t.Error("should not be processed initially")
	}
	MarkProcessed("r/r", 1, "job-1")
	ok, _ = IsProcessed("r/r", 1)
	if !ok {
		t.Error("should be processed after mark")
	}
}

func TestRepoMemory(t *testing.T) {
	setupTestDB(t)
	SetRepoMemory("r/r", "desc", "a test project")
	SetRepoMemory("r/r", "lang", "Go")

	mem, _ := GetRepoMemory("r/r", nil)
	if len(mem) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(mem))
	}
	if mem["desc"] != "a test project" {
		t.Errorf("desc = %q", mem["desc"])
	}

	// Get specific key
	key := "desc"
	mem2, _ := GetRepoMemory("r/r", &key)
	if len(mem2) != 1 || mem2["desc"] != "a test project" {
		t.Errorf("specific key lookup failed: %v", mem2)
	}

	// Upsert
	SetRepoMemory("r/r", "desc", "updated")
	mem3, _ := GetRepoMemory("r/r", &key)
	if mem3["desc"] != "updated" {
		t.Errorf("upsert failed: %q", mem3["desc"])
	}
}

func TestDeploymentPlan(t *testing.T) {
	setupTestDB(t)

	// Not found
	p, _ := GetDeploymentPlan("r/r")
	if p != nil {
		t.Error("expected nil for missing plan")
	}

	SaveDeploymentPlan("r/r", `{"options":[]}`)
	p, _ = GetDeploymentPlan("r/r")
	if p == nil || p.Status != "analyzed" {
		t.Fatalf("plan not found or wrong status")
	}
	if p.PlanJSON != `{"options":[]}` {
		t.Errorf("plan_json = %q", p.PlanJSON)
	}

	UpdateDeploymentPlanStatus("r/r", "tested")
	p, _ = GetDeploymentPlan("r/r")
	if p.Status != "tested" {
		t.Errorf("status = %q, want tested", p.Status)
	}
}

func TestChatHistory(t *testing.T) {
	setupTestDB(t)
	AddChatMessage("r/r", 1, "user", "hello")
	AddChatMessage("r/r", 1, "ai", "hi there")
	AddChatMessage("r/r", 2, "user", "other issue")

	msgs, _ := GetChatHistory("r/r", 1)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Errorf("first msg: %+v", msgs[0])
	}
	if msgs[1].Role != "ai" {
		t.Errorf("second msg role: %q", msgs[1].Role)
	}

	ClearChatHistory("r/r", 1)
	msgs, _ = GetChatHistory("r/r", 1)
	if len(msgs) != 0 {
		t.Errorf("expected 0 after clear, got %d", len(msgs))
	}
	// Other issue unaffected
	msgs2, _ := GetChatHistory("r/r", 2)
	if len(msgs2) != 1 {
		t.Errorf("issue 2 should still have 1 msg, got %d", len(msgs2))
	}
}

func TestDeleteRepoData(t *testing.T) {
	setupTestDB(t)
	SaveDeploymentPlan("r/r", `{}`)
	SetRepoMemory("r/r", "k", "v")
	MarkProcessed("r/r", 1, "j")

	DeleteRepoData("r/r")

	p, _ := GetDeploymentPlan("r/r")
	if p != nil {
		t.Error("plan should be deleted")
	}
	mem, _ := GetRepoMemory("r/r", nil)
	if len(mem) != 0 {
		t.Error("memory should be deleted")
	}
	ok, _ := IsProcessed("r/r", 1)
	if ok {
		t.Error("processed should be deleted")
	}
}

func TestGetStats(t *testing.T) {
	setupTestDB(t)
	MarkProcessed("r/r", 1, "j")
	AddRun(Run{Repo: "r/r", Issue: 1, Verdict: "BUG_CONFIRMED", CreatedAt: "2026-01-01"})
	AddRun(Run{Repo: "r/r", Issue: 2, Verdict: "FEATURE_REQUEST", CreatedAt: "2026-01-02"})

	s, _ := GetStats("r/r")
	if s.Processed != 1 {
		t.Errorf("processed = %d", s.Processed)
	}
	if s.TotalRuns != 2 {
		t.Errorf("total_runs = %d", s.TotalRuns)
	}
	if s.Bugs != 1 {
		t.Errorf("bugs = %d", s.Bugs)
	}
	if s.Features != 1 {
		t.Errorf("features = %d", s.Features)
	}
}

func TestGetTotalStats(t *testing.T) {
	setupTestDB(t)
	AddRun(Run{Repo: "a/a", Issue: 1, Verdict: "BUG_CONFIRMED", CreatedAt: "2026-01-01"})
	AddRun(Run{Repo: "b/b", Issue: 2, Verdict: "NOT_REPRODUCIBLE", CreatedAt: "2026-01-02"})
	AddRun(Run{Repo: "c/c", Issue: 3, Verdict: "BUG_CONFIRMED", CreatedAt: "2026-01-03"})
	MarkProcessed("a/a", 1, "j")

	s, _ := GetTotalStats()
	if s.TotalRuns != 3 {
		t.Errorf("total_runs = %d, want 3", s.TotalRuns)
	}
	if s.TotalProcessed != 1 {
		t.Errorf("total_processed = %d, want 1", s.TotalProcessed)
	}
	if s.BugsConfirmed != 2 {
		t.Errorf("bugs_confirmed = %d, want 2", s.BugsConfirmed)
	}
	if s.NotReproducible != 1 {
		t.Errorf("not_reproducible = %d, want 1", s.NotReproducible)
	}
}
