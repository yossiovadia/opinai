package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVerdict_StructuredBlock(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantV      string
		wantC      string
	}{
		{
			name: "basic verdict block",
			text: `I've investigated the bug thoroughly.

===VERDICT===
verdict: BUG_CONFIRMED
confidence: HIGH
===END_VERDICT===`,
			wantV: "BUG_CONFIRMED",
			wantC: "HIGH",
		},
		{
			name: "not reproducible with medium confidence",
			text: `The tests passed.

===VERDICT===
verdict: NOT_REPRODUCIBLE
confidence: MEDIUM
===END_VERDICT===`,
			wantV: "NOT_REPRODUCIBLE",
			wantC: "MEDIUM",
		},
		{
			name: "inconclusive",
			text: `===VERDICT===
verdict: INCONCLUSIVE
confidence: LOW
===END_VERDICT===`,
			wantV: "INCONCLUSIVE",
			wantC: "LOW",
		},
		{
			name: "missing end marker",
			text: `===VERDICT===
verdict: BUG_CONFIRMED
confidence: HIGH`,
			wantV: "BUG_CONFIRMED",
			wantC: "HIGH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, c := parseVerdict(tt.text)
			if v != tt.wantV {
				t.Errorf("verdict = %q, want %q", v, tt.wantV)
			}
			if c != tt.wantC {
				t.Errorf("confidence = %q, want %q", c, tt.wantC)
			}
		})
	}
}

func TestParseVerdict_FallbackKeywords(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		wantV string
		wantC string
	}{
		{
			name:  "keyword BUG_CONFIRMED",
			text:  "After analysis, this is clearly BUG_CONFIRMED with high confidence.",
			wantV: "BUG_CONFIRMED",
			wantC: "LOW", // no structured confidence
		},
		{
			name:  "keyword NOT_REPRODUCIBLE with confidence",
			text:  "The bug is NOT_REPRODUCIBLE. Confidence: HIGH",
			wantV: "NOT_REPRODUCIBLE",
			wantC: "HIGH",
		},
		{
			name:  "no keywords defaults to INCONCLUSIVE LOW",
			text:  "I couldn't determine anything.",
			wantV: "INCONCLUSIVE",
			wantC: "LOW",
		},
		{
			name:  "empty text",
			text:  "",
			wantV: "INCONCLUSIVE",
			wantC: "LOW",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, c := parseVerdict(tt.text)
			if v != tt.wantV {
				t.Errorf("verdict = %q, want %q", v, tt.wantV)
			}
			if c != tt.wantC {
				t.Errorf("confidence = %q, want %q", c, tt.wantC)
			}
		})
	}
}

func TestToolState_SafePath(t *testing.T) {
	repoDir := t.TempDir()
	state := &ToolState{RepoDir: repoDir}

	tests := []struct {
		path    string
		wantOK  bool
	}{
		{"main.go", true},
		{"src/app.py", true},
		{".", true},
		{"../../../etc/passwd", false},
		{"/etc/passwd", false},
	}

	for _, tt := range tests {
		result := state.safePath(tt.path)
		if tt.wantOK && result == "" {
			t.Errorf("safePath(%q) = empty, want allowed", tt.path)
		}
		if !tt.wantOK && result != "" {
			t.Errorf("safePath(%q) = %q, want blocked", tt.path, result)
		}
	}
}

func TestToolState_ReadFile(t *testing.T) {
	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "test.txt"), []byte("hello world"), 0o644)

	state := &ToolState{RepoDir: repoDir}

	// Good read
	result, isErr := state.handleReadFile(map[string]any{"path": "test.txt"})
	if isErr {
		t.Errorf("expected no error, got: %s", result)
	}
	if result != "hello world" {
		t.Errorf("result = %q", result)
	}
	if len(state.FilesRead) != 1 || state.FilesRead[0] != "test.txt" {
		t.Errorf("FilesRead = %v", state.FilesRead)
	}

	// Missing file
	result2, isErr2 := state.handleReadFile(map[string]any{"path": "nonexistent.txt"})
	if !isErr2 {
		t.Error("expected error for missing file")
	}
	_ = result2

	// Path traversal
	result3, isErr3 := state.handleReadFile(map[string]any{"path": "../../../etc/passwd"})
	if !isErr3 {
		t.Error("expected error for path traversal")
	}
	_ = result3

	// Empty path
	result4, isErr4 := state.handleReadFile(map[string]any{"path": ""})
	if !isErr4 {
		t.Error("expected error for empty path")
	}
	_ = result4
}

func TestToolState_ListDir(t *testing.T) {
	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "file1.go"), []byte("package main"), 0o644)
	os.Mkdir(filepath.Join(repoDir, "subdir"), 0o755)

	state := &ToolState{RepoDir: repoDir}

	result, isErr := state.handleListDir(map[string]any{"path": "."})
	if isErr {
		t.Errorf("unexpected error: %s", result)
	}
	if result == "" || result == "(empty directory)" {
		t.Error("expected non-empty listing")
	}
}

func TestToolState_Grep(t *testing.T) {
	repoDir := t.TempDir()
	os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\nfunc bug() {}\n"), 0o644)

	state := &ToolState{RepoDir: repoDir}

	result, isErr := state.handleGrep(map[string]any{"pattern": "bug"})
	if isErr {
		t.Errorf("unexpected error: %s", result)
	}
	if result == "(no matches found)" {
		t.Error("expected matches")
	}

	// No matches
	result2, isErr2 := state.handleGrep(map[string]any{"pattern": "zzz_nonexistent_zzz"})
	if isErr2 {
		t.Errorf("no-match should not be an error: %s", result2)
	}
	if result2 != "(no matches found)" {
		t.Errorf("expected no matches message, got: %s", result2)
	}

	// Path traversal
	result3, isErr3 := state.handleGrep(map[string]any{"pattern": "test", "path": "/etc"})
	if !isErr3 {
		t.Error("expected error for path outside repo")
	}
	_ = result3
}

func TestToolState_RunTestLimit(t *testing.T) {
	state := &ToolState{RepoDir: t.TempDir(), TestRuns: maxTestRuns}

	result, isErr := state.handleRunTest(map[string]any{"script": "print('hello')"})
	if !isErr {
		t.Error("expected error when max test runs reached")
	}
	if result == "" {
		t.Error("expected error message")
	}
}

func TestToolState_ServerRequestNoServer(t *testing.T) {
	state := &ToolState{RepoDir: t.TempDir(), ServerURL: ""}

	result, isErr := state.handleServerRequest(map[string]any{
		"method": "GET", "path": "/health",
	})
	if !isErr {
		t.Error("expected error when no server")
	}
	_ = result
}

func TestParsePRVerdict_StructuredBlock(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		wantV string
		wantR string
	}{
		{
			name: "approve with low risk",
			text: `Code looks good.

===PR_VERDICT===
verdict: APPROVE
risk: LOW
===END_PR_VERDICT===`,
			wantV: "APPROVE",
			wantR: "LOW",
		},
		{
			name: "changes requested high risk",
			text: `Found issues.

===PR_VERDICT===
verdict: CHANGES_REQUESTED
risk: HIGH
===END_PR_VERDICT===`,
			wantV: "CHANGES_REQUESTED",
			wantR: "HIGH",
		},
		{
			name: "comment medium risk",
			text: `===PR_VERDICT===
verdict: COMMENT
risk: MEDIUM
===END_PR_VERDICT===`,
			wantV: "COMMENT",
			wantR: "MEDIUM",
		},
		{
			name: "critical risk",
			text: `===PR_VERDICT===
verdict: CHANGES_REQUESTED
risk: CRITICAL
===END_PR_VERDICT===`,
			wantV: "CHANGES_REQUESTED",
			wantR: "CRITICAL",
		},
		{
			name: "missing end marker",
			text: `===PR_VERDICT===
verdict: APPROVE
risk: LOW`,
			wantV: "APPROVE",
			wantR: "LOW",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, r := parsePRVerdict(tt.text)
			if v != tt.wantV {
				t.Errorf("verdict = %q, want %q", v, tt.wantV)
			}
			if r != tt.wantR {
				t.Errorf("risk = %q, want %q", r, tt.wantR)
			}
		})
	}
}

func TestParsePRVerdict_FallbackKeywords(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		wantV string
		wantR string
	}{
		{
			name:  "keyword CHANGES_REQUESTED",
			text:  "This PR has issues: CHANGES_REQUESTED. Risk: HIGH",
			wantV: "CHANGES_REQUESTED",
			wantR: "HIGH",
		},
		{
			name:  "keyword APPROVE",
			text:  "APPROVE this PR.",
			wantV: "APPROVE",
			wantR: "LOW",
		},
		{
			name:  "no keywords defaults to COMMENT LOW",
			text:  "Some observations about the code.",
			wantV: "COMMENT",
			wantR: "LOW",
		},
		{
			name:  "empty text",
			text:  "",
			wantV: "COMMENT",
			wantR: "LOW",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, r := parsePRVerdict(tt.text)
			if v != tt.wantV {
				t.Errorf("verdict = %q, want %q", v, tt.wantV)
			}
			if r != tt.wantR {
				t.Errorf("risk = %q, want %q", r, tt.wantR)
			}
		})
	}
}

func TestExtractPRReview(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "with markers",
			text: `Some preamble

--- OPINAI PR REVIEW ---
## Summary
This PR adds feature X.

## Verdict
Looks good.
--- END PR REVIEW ---

Some epilogue`,
			want: "## Summary\nThis PR adds feature X.\n\n## Verdict\nLooks good.",
		},
		{
			name: "no end marker",
			text: `--- OPINAI PR REVIEW ---
Review content here.`,
			want: "Review content here.",
		},
		{
			name: "no markers",
			text: "Just regular text.",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRReview(tt.text)
			if got != tt.want {
				t.Errorf("extractPRReview() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolDefs(t *testing.T) {
	defs := ToolDefs()
	if len(defs) != 5 {
		t.Errorf("expected 5 tool definitions, got %d", len(defs))
	}

	names := make(map[string]bool)
	for _, d := range defs {
		names[d.Name] = true
		if d.Description == "" {
			t.Errorf("tool %q has empty description", d.Name)
		}
		if d.InputSchema == nil {
			t.Errorf("tool %q has nil input_schema", d.Name)
		}
	}

	expected := []string{"read_file", "list_dir", "grep", "run_test", "server_request"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing tool definition: %s", name)
		}
	}
}
