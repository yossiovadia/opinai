package controller

import (
	"testing"
)

func TestExtractPlanResources(t *testing.T) {
	// Empty plan
	r := extractPlanResources("")
	if r.CPUReq != "200m" || r.MemReq != "512Mi" || r.TimeoutMinutes != 10 {
		t.Errorf("empty defaults: %+v", r)
	}

	// Full plan
	plan := `{"install_command":"pip install --user foo","resource_requirements":{"cpu":"500m","memory":"1Gi"},"job_timeout_minutes":20}`
	r = extractPlanResources(plan)
	if r.InstallCommand != "pip install --user foo" {
		t.Errorf("install = %q", r.InstallCommand)
	}
	if r.CPUReq != "500m" {
		t.Errorf("cpu = %q", r.CPUReq)
	}
	if r.MemReq != "1Gi" {
		t.Errorf("mem = %q", r.MemReq)
	}
	if r.TimeoutMinutes != 20 {
		t.Errorf("timeout = %d", r.TimeoutMinutes)
	}

	// Partial plan (no resources)
	r = extractPlanResources(`{"install_command":"make build"}`)
	if r.InstallCommand != "make build" {
		t.Errorf("install = %q", r.InstallCommand)
	}
	if r.CPUReq != "200m" {
		t.Errorf("partial should keep defaults: %+v", r)
	}

	// Invalid JSON
	r = extractPlanResources("not json")
	if r.CPUReq != "200m" {
		t.Errorf("invalid should use defaults: %+v", r)
	}
}

func TestPRReviewJobName(t *testing.T) {
	tests := []struct {
		repo string
		pr   int
		want string
	}{
		{"owner/repo", 42, "opinai-pr-owner-repo-42"},
		{"org/My-Project", 1, "opinai-pr-org-my-project-1"},
	}
	for _, tt := range tests {
		got := PRReviewJobName(tt.repo, tt.pr)
		if got != tt.want {
			t.Errorf("PRReviewJobName(%q, %d) = %q, want %q", tt.repo, tt.pr, got, tt.want)
		}
	}
}

func TestIsSourceFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"main.go", true},
		{"src/app.py", true},
		{"index.ts", true},
		{"README.md", true},
		{"deploy.yaml", true},
		{"vendor/pkg/foo.go", false},
		{"node_modules/dep/index.js", false},
		{"package-lock.json", false},
		{"go.sum", false},
		{"app.min.js", false},
		{"image.png", false},
		{"binary.exe", false},
	}
	for _, tt := range tests {
		got := isSourceFile(tt.name)
		if got != tt.want {
			t.Errorf("isSourceFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParsePRVerdict(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want string
	}{
		{
			name: "approve from marker",
			logs: "some output\n--- OPINAI PR VERDICT: APPROVE ---\n",
			want: "APPROVE",
		},
		{
			name: "changes_requested from marker",
			logs: "--- OPINAI PR VERDICT: CHANGES_REQUESTED ---",
			want: "CHANGES_REQUESTED",
		},
		{
			name: "comment from marker",
			logs: "--- OPINAI PR VERDICT: COMMENT ---",
			want: "COMMENT",
		},
		{
			name: "fallback keyword",
			logs: "The review determined CHANGES_REQUESTED for this PR.",
			want: "CHANGES_REQUESTED",
		},
		{
			name: "no markers defaults to COMMENT",
			logs: "some random log output",
			want: "COMMENT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePRVerdict(tt.logs)
			if got != tt.want {
				t.Errorf("parsePRVerdict() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePRRisk(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want string
	}{
		{
			name: "high from marker",
			logs: "--- OPINAI PR RISK: HIGH ---",
			want: "HIGH",
		},
		{
			name: "critical from marker",
			logs: "--- OPINAI PR RISK: CRITICAL ---",
			want: "CRITICAL",
		},
		{
			name: "medium from verdict block",
			logs: "===PR_VERDICT===\nrisk: MEDIUM\n===END_PR_VERDICT===",
			want: "MEDIUM",
		},
		{
			name: "defaults to LOW",
			logs: "some log output",
			want: "LOW",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePRRisk(tt.logs)
			if got != tt.want {
				t.Errorf("parsePRRisk() = %q, want %q", got, tt.want)
			}
		})
	}
}

