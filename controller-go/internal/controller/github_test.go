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
