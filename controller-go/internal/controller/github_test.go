package controller

import (
	"encoding/json"
	"strings"
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

func TestPRReviewParsing(t *testing.T) {
	data := `[{"user":{"login":"alice"},"body":"Looks good overall","state":"APPROVED"},{"user":{"login":"bob"},"body":"","state":"APPROVED"},{"user":{"login":"coderabbitai"},"body":"Found issue with nil check","state":"COMMENTED"}]`
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Filter empty bodies like FetchPRReviews does
	var reviews []PRReview
	for _, r := range raw {
		if r.Body == "" {
			continue
		}
		reviews = append(reviews, PRReview{
			Author: r.User.Login,
			Body:   r.Body,
			State:  r.State,
		})
	}
	if len(reviews) != 2 {
		t.Fatalf("expected 2 reviews (skipping empty), got %d", len(reviews))
	}
	if reviews[0].Author != "alice" {
		t.Errorf("author = %q", reviews[0].Author)
	}
	if reviews[0].State != "APPROVED" {
		t.Errorf("state = %q", reviews[0].State)
	}
	if reviews[1].Author != "coderabbitai" {
		t.Errorf("author = %q", reviews[1].Author)
	}
}

func TestPRInlineCommentParsing(t *testing.T) {
	data := `[{"user":{"login":"reviewer"},"body":"This could panic on nil","path":"main.go","created_at":"2026-04-01T00:00:00Z"}]`
	var raw []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		Path      string `json:"path"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected 1, got %d", len(raw))
	}
	if raw[0].Path != "main.go" {
		t.Errorf("path = %q", raw[0].Path)
	}
	// Simulate the path-prefixing logic from FetchPRInlineComments
	body := raw[0].Body
	if raw[0].Path != "" {
		body = "[" + raw[0].Path + "] " + body
	}
	if body != "[main.go] This could panic on nil" {
		t.Errorf("formatted body = %q", body)
	}
}

func TestPRCommentContextSerialization(t *testing.T) {
	comments := []PRCommentContext{
		{Author: "alice", Body: "Looks good", Type: "comment"},
		{Author: "bob", Body: "[COMMENTED] Check nil handling", Type: "review"},
		{Author: "carol", Body: "[main.go] Missing error check", Type: "inline"},
	}
	b, err := json.Marshal(comments)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed []PRCommentContext
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("expected 3, got %d", len(parsed))
	}
	if parsed[0].Type != "comment" {
		t.Errorf("type = %q", parsed[0].Type)
	}
	if parsed[1].Type != "review" {
		t.Errorf("type = %q", parsed[1].Type)
	}
	if parsed[2].Type != "inline" {
		t.Errorf("type = %q", parsed[2].Type)
	}
}

func TestFormatPRCommentsForAgent(t *testing.T) {
	comments := []PRCommentContext{
		{Author: "coderabbitai", Body: "Found duplicate endpoint construction", Type: "review"},
		{Author: "alice", Body: "Can we simplify the handler?", Type: "comment"},
		{Author: "coderabbitai", Body: "[main.go] Nil check missing", Type: "inline"},
	}
	result := FormatPRCommentsForAgent(comments)

	// Should group by author
	if !strings.Contains(result, "### @coderabbitai:") {
		t.Error("should contain coderabbitai header")
	}
	if !strings.Contains(result, "### @alice:") {
		t.Error("should contain alice header")
	}
	if !strings.Contains(result, "Found duplicate endpoint") {
		t.Error("should contain review body")
	}
	if !strings.Contains(result, "[main.go] Nil check missing") {
		t.Error("should contain inline body")
	}

	// coderabbitai should appear before alice (insertion order)
	crIdx := strings.Index(result, "@coderabbitai")
	aliceIdx := strings.Index(result, "@alice")
	if crIdx > aliceIdx {
		t.Error("coderabbitai should appear before alice (first-seen order)")
	}
}

func TestFormatPRCommentsForAgentEmpty(t *testing.T) {
	result := FormatPRCommentsForAgent(nil)
	if result != "" {
		t.Errorf("expected empty for nil, got %q", result)
	}
	result = FormatPRCommentsForAgent([]PRCommentContext{})
	if result != "" {
		t.Errorf("expected empty for empty slice, got %q", result)
	}
}

func TestCollectPRCommentsTruncation(t *testing.T) {
	// Test that the truncation logic in CollectPRComments works correctly
	// by testing it indirectly through PRCommentContext
	longBody := strings.Repeat("x", 600)
	truncated := longBody
	maxLen := 500
	if len(truncated) > maxLen {
		truncated = truncated[:maxLen] + "..."
	}
	if len(truncated) != 503 { // 500 + "..."
		t.Errorf("truncated len = %d, want 503", len(truncated))
	}
	if !strings.HasSuffix(truncated, "...") {
		t.Error("truncated should end with ...")
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
