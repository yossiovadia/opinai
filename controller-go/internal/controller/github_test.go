package controller

import (
	"encoding/json"
	"testing"
)

func TestIssueStateParsing(t *testing.T) {
	data := `{"number":42,"title":"Test Bug","body":"description","state":"closed","created_at":"2026-04-01T00:00:00Z"}`
	var issue Issue
	if err := json.Unmarshal([]byte(data), &issue); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if issue.State != "closed" {
		t.Errorf("state = %q, want closed", issue.State)
	}
	if issue.Number != 42 {
		t.Errorf("number = %d", issue.Number)
	}
	if issue.CreatedAt != "2026-04-01T00:00:00Z" {
		t.Errorf("created_at = %q", issue.CreatedAt)
	}
}

func TestIssueSkipsPRs(t *testing.T) {
	data := `[
		{"number":1,"title":"Issue","pull_request":null},
		{"number":2,"title":"PR","pull_request":{"url":"..."}},
		{"number":3,"title":"Issue2"}
	]`
	var issues []Issue
	json.Unmarshal([]byte(data), &issues)

	var filtered []Issue
	for _, i := range issues {
		if i.PullRequest == nil {
			filtered = append(filtered, i)
		}
	}
	if len(filtered) != 2 {
		t.Errorf("expected 2 non-PR issues, got %d", len(filtered))
	}
}

func TestPullRequestParsing(t *testing.T) {
	data := `{"number":99,"title":"Fix bug","body":"Fixes #42","state":"open","head":{"ref":"fix-branch","sha":"abc123"},"base":{"ref":"main"},"user":{"login":"alice"},"created_at":"2026-04-01T00:00:00Z"}`
	var pr PullRequest
	if err := json.Unmarshal([]byte(data), &pr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pr.Number != 99 {
		t.Errorf("number = %d", pr.Number)
	}
	if pr.Title != "Fix bug" {
		t.Errorf("title = %q", pr.Title)
	}
	if pr.Head.Ref != "fix-branch" {
		t.Errorf("head.ref = %q", pr.Head.Ref)
	}
	if pr.Head.SHA != "abc123" {
		t.Errorf("head.sha = %q", pr.Head.SHA)
	}
	if pr.Base.Ref != "main" {
		t.Errorf("base.ref = %q", pr.Base.Ref)
	}
	if pr.User.Login != "alice" {
		t.Errorf("user.login = %q", pr.User.Login)
	}
}

func TestPRChangedFileParsing(t *testing.T) {
	data := `[{"filename":"main.go","status":"modified","additions":10,"deletions":5,"patch":"@@ -1,5 +1,10 @@\n+new code"},{"filename":"README.md","status":"added","additions":20,"deletions":0}]`
	var files []PRChangedFile
	if err := json.Unmarshal([]byte(data), &files); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0].Filename != "main.go" {
		t.Errorf("filename = %q", files[0].Filename)
	}
	if files[0].Status != "modified" {
		t.Errorf("status = %q", files[0].Status)
	}
	if files[0].Additions != 10 {
		t.Errorf("additions = %d", files[0].Additions)
	}
	if files[0].Patch == "" {
		t.Error("expected non-empty patch")
	}
}

func TestJobName(t *testing.T) {
	name := JobName("owner/repo", 42)
	if name != "opinai-owner-repo-42" {
		t.Errorf("JobName = %q", name)
	}
	name2 := JobName("org/My-Project", 1)
	if name2 != "opinai-org-my-project-1" {
		t.Errorf("JobName = %q", name2)
	}
}
