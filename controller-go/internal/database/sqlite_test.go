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

func TestPRReviewTable(t *testing.T) {
	setupTestDB(t)
	// Verify table exists
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='pr_reviews'").Scan(&name)
	if err != nil {
		t.Fatalf("pr_reviews table not found: %v", err)
	}
}

func TestAddPRReviewGetPRReviews(t *testing.T) {
	setupTestDB(t)
	id, err := AddPRReview(PRReview{
		Repo: "owner/repo", PRNumber: 10, Title: "Fix bug",
		Author: "alice", Verdict: "APPROVE", Risk: "LOW",
		ReviewText: "LGTM", Duration: "3s",
		CreatedAt: "2026-04-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("AddPRReview: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	reviews, err := GetPRReviews("owner/repo", 10)
	if err != nil {
		t.Fatalf("GetPRReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review, got %d", len(reviews))
	}
	r := reviews[0]
	if r.PRNumber != 10 {
		t.Errorf("pr_number = %d, want 10", r.PRNumber)
	}
	if r.Verdict != "APPROVE" {
		t.Errorf("verdict = %q, want APPROVE", r.Verdict)
	}
	if r.Risk != "LOW" {
		t.Errorf("risk = %q, want LOW", r.Risk)
	}
	if r.Author != "alice" {
		t.Errorf("author = %q, want alice", r.Author)
	}
	if r.Posted {
		t.Error("should not be posted initially")
	}

	// Filter by different repo
	reviews2, _ := GetPRReviews("other/repo", 10)
	if len(reviews2) != 0 {
		t.Errorf("expected 0 reviews for other repo, got %d", len(reviews2))
	}

	// No filter
	allReviews, _ := GetPRReviews("", 10)
	if len(allReviews) != 1 {
		t.Errorf("expected 1 total review, got %d", len(allReviews))
	}
}

func TestGetPRReview(t *testing.T) {
	setupTestDB(t)
	id, _ := AddPRReview(PRReview{Repo: "r/r", PRNumber: 5, CreatedAt: "2026-01-01"})
	review, err := GetPRReview(id)
	if err != nil || review == nil {
		t.Fatalf("GetPRReview(%d): %v", id, err)
	}
	if review.PRNumber != 5 {
		t.Errorf("pr_number = %d", review.PRNumber)
	}

	// Non-existent
	r2, _ := GetPRReview(9999)
	if r2 != nil {
		t.Error("expected nil for non-existent review")
	}
}

func TestGetPRReviewsByPR(t *testing.T) {
	setupTestDB(t)
	AddPRReview(PRReview{Repo: "r/r", PRNumber: 7, Verdict: "COMMENT", CreatedAt: "2026-01-01"})
	AddPRReview(PRReview{Repo: "r/r", PRNumber: 7, Verdict: "APPROVE", CreatedAt: "2026-01-02"})
	AddPRReview(PRReview{Repo: "r/r", PRNumber: 8, Verdict: "APPROVE", CreatedAt: "2026-01-03"})

	reviews, _ := GetPRReviewsByPR("r/r", 7)
	if len(reviews) != 2 {
		t.Fatalf("expected 2 reviews for PR 7, got %d", len(reviews))
	}
	// Newest first
	if reviews[0].Verdict != "APPROVE" {
		t.Errorf("first review verdict = %q, want APPROVE (newest)", reviews[0].Verdict)
	}
}

func TestPendingPRTable(t *testing.T) {
	setupTestDB(t)
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='pending_pr_reviews'").Scan(&name)
	if err != nil {
		t.Fatalf("pending_pr_reviews table not found: %v", err)
	}
}

func TestAddPendingPR(t *testing.T) {
	setupTestDB(t)
	if err := AddPendingPR("owner/repo", 10, "Fix bug"); err != nil {
		t.Fatalf("AddPendingPR: %v", err)
	}
	items := GetAllPendingPRs()
	if len(items) != 1 {
		t.Fatalf("expected 1 pending PR, got %d", len(items))
	}
	if items[0].Repo != "owner/repo" || items[0].PRNumber != 10 || items[0].Title != "Fix bug" {
		t.Errorf("unexpected item: %+v", items[0])
	}
}

func TestAddPendingPRDuplicate(t *testing.T) {
	setupTestDB(t)
	AddPendingPR("owner/repo", 10, "Fix bug")
	// Duplicate insert should be ignored (INSERT OR IGNORE)
	if err := AddPendingPR("owner/repo", 10, "Updated title"); err != nil {
		t.Fatalf("duplicate AddPendingPR: %v", err)
	}
	items := GetAllPendingPRs()
	if len(items) != 1 {
		t.Fatalf("expected 1 pending PR after duplicate, got %d", len(items))
	}
	// Title should remain original since INSERT OR IGNORE
	if items[0].Title != "Fix bug" {
		t.Errorf("title = %q, want original 'Fix bug'", items[0].Title)
	}
}

func TestDeletePendingPR(t *testing.T) {
	setupTestDB(t)
	AddPendingPR("owner/repo", 10, "Fix bug")
	AddPendingPR("owner/repo", 11, "Add feature")
	DeletePendingPR("owner/repo", 10)
	items := GetAllPendingPRs()
	if len(items) != 1 {
		t.Fatalf("expected 1 pending PR after delete, got %d", len(items))
	}
	if items[0].PRNumber != 11 {
		t.Errorf("remaining PR number = %d, want 11", items[0].PRNumber)
	}
}

func TestDeletePendingPRNonExistent(t *testing.T) {
	setupTestDB(t)
	// Should not panic or error on non-existent entry
	DeletePendingPR("owner/repo", 999)
}

func TestGetPendingPRs(t *testing.T) {
	setupTestDB(t)
	AddPendingPR("owner/repo", 10, "Fix bug")
	AddPendingPR("owner/repo", 11, "Add feature")
	AddPendingPR("other/repo", 5, "Other PR")

	items := GetPendingPRs("owner/repo")
	if len(items) != 2 {
		t.Fatalf("expected 2 pending PRs for owner/repo, got %d", len(items))
	}

	items2 := GetPendingPRs("other/repo")
	if len(items2) != 1 {
		t.Fatalf("expected 1 pending PR for other/repo, got %d", len(items2))
	}

	items3 := GetPendingPRs("nonexistent/repo")
	if len(items3) != 0 {
		t.Errorf("expected 0 pending PRs for nonexistent repo, got %d", len(items3))
	}
}

func TestGetAllPendingPRs(t *testing.T) {
	setupTestDB(t)
	AddPendingPR("a/repo", 1, "PR 1")
	AddPendingPR("b/repo", 2, "PR 2")
	AddPendingPR("a/repo", 3, "PR 3")

	items := GetAllPendingPRs()
	if len(items) != 3 {
		t.Fatalf("expected 3 total pending PRs, got %d", len(items))
	}
}

func TestGetAllPendingPRsEmpty(t *testing.T) {
	setupTestDB(t)
	items := GetAllPendingPRs()
	if len(items) != 0 {
		t.Errorf("expected 0 pending PRs on empty table, got %d", len(items))
	}
}

func TestMarkPRReviewPosted(t *testing.T) {
	setupTestDB(t)
	id, _ := AddPRReview(PRReview{Repo: "r/r", PRNumber: 1, CreatedAt: "2026-01-01"})
	review, _ := GetPRReview(id)
	if review.Posted {
		t.Error("should not be posted initially")
	}
	MarkPRReviewPosted(id)
	review, _ = GetPRReview(id)
	if !review.Posted {
		t.Error("should be posted after MarkPRReviewPosted")
	}
}

func TestDeduplicatePRReviews(t *testing.T) {
	setupTestDB(t)

	// Insert duplicates: 3 reviews for PR 703, 2 for PR 709, 1 for PR 713
	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 703, Verdict: "COMMENT", CreatedAt: "2026-01-01T00:00:00Z"})
	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 703, Verdict: "APPROVE", CreatedAt: "2026-01-01T00:01:00Z"})
	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 703, Verdict: "APPROVE", CreatedAt: "2026-01-01T00:02:00Z"})

	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 709, Verdict: "CHANGES_REQUESTED", CreatedAt: "2026-01-02T00:00:00Z"})
	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 709, Verdict: "CHANGES_REQUESTED", CreatedAt: "2026-01-02T00:01:00Z"})

	AddPRReview(PRReview{Repo: "owner/repo", PRNumber: 713, Verdict: "APPROVE", CreatedAt: "2026-01-03T00:00:00Z"})

	// Verify duplicates exist
	reviews703, _ := GetPRReviewsByPR("owner/repo", 703)
	if len(reviews703) != 3 {
		t.Fatalf("expected 3 reviews for PR 703 before dedup, got %d", len(reviews703))
	}

	// Run dedup
	removed, err := DeduplicatePRReviews()
	if err != nil {
		t.Fatalf("DeduplicatePRReviews: %v", err)
	}
	if removed != 3 {
		t.Errorf("expected 3 removed, got %d", removed)
	}

	// Verify: 1 per PR
	reviews703, _ = GetPRReviewsByPR("owner/repo", 703)
	if len(reviews703) != 1 {
		t.Errorf("expected 1 review for PR 703 after dedup, got %d", len(reviews703))
	}
	reviews709, _ := GetPRReviewsByPR("owner/repo", 709)
	if len(reviews709) != 1 {
		t.Errorf("expected 1 review for PR 709 after dedup, got %d", len(reviews709))
	}
	reviews713, _ := GetPRReviewsByPR("owner/repo", 713)
	if len(reviews713) != 1 {
		t.Errorf("expected 1 review for PR 713 after dedup, got %d", len(reviews713))
	}

	// The kept review should be the one with the highest ID (latest inserted)
	if reviews703[0].Verdict != "APPROVE" {
		t.Errorf("expected latest review (APPROVE) to be kept, got %q", reviews703[0].Verdict)
	}

	// Running dedup again should remove nothing
	removed2, err := DeduplicatePRReviews()
	if err != nil {
		t.Fatalf("DeduplicatePRReviews (2nd run): %v", err)
	}
	if removed2 != 0 {
		t.Errorf("expected 0 removed on 2nd run, got %d", removed2)
	}
}

func TestGetTotalStatsIncludesPRReviews(t *testing.T) {
	setupTestDB(t)

	AddPRReview(PRReview{Repo: "r/r", PRNumber: 1, Verdict: "APPROVE", CreatedAt: "2026-01-01"})
	AddPRReview(PRReview{Repo: "r/r", PRNumber: 2, Verdict: "CHANGES_REQUESTED", CreatedAt: "2026-01-02"})
	AddPRReview(PRReview{Repo: "r/r", PRNumber: 3, Verdict: "COMMENT", CreatedAt: "2026-01-03"})

	stats, err := GetTotalStats()
	if err != nil {
		t.Fatalf("GetTotalStats: %v", err)
	}
	if stats.PRsReviewed != 3 {
		t.Errorf("PRsReviewed = %d, want 3", stats.PRsReviewed)
	}
	if stats.PRsApproved != 1 {
		t.Errorf("PRsApproved = %d, want 1", stats.PRsApproved)
	}
	if stats.PRsChangesReq != 1 {
		t.Errorf("PRsChangesReq = %d, want 1", stats.PRsChangesReq)
	}
	if stats.PRsCommented != 1 {
		t.Errorf("PRsCommented = %d, want 1", stats.PRsCommented)
	}
}
