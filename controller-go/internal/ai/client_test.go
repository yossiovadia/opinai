package ai

import (
	"os"
	"testing"
)

func TestSanitize(t *testing.T) {
	os.Setenv("AI_API_KEY", "sk-secret-key-12345")
	os.Setenv("GITHUB_TOKEN", "ghp_token_abcdef123")
	defer os.Unsetenv("AI_API_KEY")
	defer os.Unsetenv("GITHUB_TOKEN")

	text := "The key is sk-secret-key-12345 and token is ghp_token_abcdef123"
	result := Sanitize(text)
	if result != "The key is REDACTED and token is REDACTED" {
		t.Errorf("sanitize failed: %q", result)
	}

	// Short secrets should not be redacted
	os.Setenv("AI_API_KEY", "short")
	result2 := Sanitize("key is short")
	if result2 != "key is short" {
		t.Errorf("short key should not be redacted: %q", result2)
	}
}

func TestParseAIJSON(t *testing.T) {
	// Tier 1: valid JSON
	result, err := ParseAIJSON(`{"options": [{"id": "full"}]}`)
	if err != nil {
		t.Fatalf("valid JSON failed: %v", err)
	}
	opts, ok := result["options"].([]any)
	if !ok || len(opts) != 1 {
		t.Errorf("expected 1 option, got %v", result["options"])
	}

	// With markdown fences
	result2, _ := ParseAIJSON("```json\n{\"options\": []}\n```")
	if _, ok := result2["options"]; !ok {
		t.Error("should strip markdown fences")
	}

	// Tier 2: truncated JSON
	result3, _ := ParseAIJSON(`{"options": [{"id": "full", "name": "Full"`)
	if result3 == nil {
		t.Fatal("truncated JSON should be repaired")
	}
	if _, ok := result3["_warning"]; !ok {
		t.Error("repaired JSON should have _warning")
	}

	// Tier 3: garbage — returns result with options key (may be empty array or nil)
	result4, _ := ParseAIJSON("this is not json at all")
	if result4 == nil {
		t.Fatal("garbage should still return a result")
	}
	if _, ok := result4["_warning"]; !ok {
		t.Error("garbage should have _warning")
	}
}

func TestLoadConfig(t *testing.T) {
	os.Setenv("AI_PROVIDER", "vertex")
	os.Setenv("AI_PROJECT", "my-project")
	os.Setenv("AI_REGION", "us-east5")
	defer func() {
		os.Unsetenv("AI_PROVIDER")
		os.Unsetenv("AI_PROJECT")
		os.Unsetenv("AI_REGION")
	}()

	cfg := LoadConfig()
	if cfg.Provider != ProviderVertex {
		t.Errorf("provider = %d, want Vertex", cfg.Provider)
	}
	if !cfg.Available() {
		t.Error("vertex with project+region should be available")
	}

	// No config
	os.Unsetenv("AI_PROVIDER")
	os.Unsetenv("AI_PROJECT")
	os.Unsetenv("AI_API_KEY")
	cfg2 := LoadConfig()
	if cfg2.Available() {
		t.Error("should not be available without key or project")
	}
}

func TestCategorizeNoAI(t *testing.T) {
	os.Unsetenv("AI_API_KEY")
	os.Unsetenv("AI_PROVIDER")
	result := Categorize("bug title", "bug body")
	if result != "BUG" {
		t.Errorf("no-AI categorize should default to BUG, got %q", result)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Error("short string should not be truncated")
	}
	if truncate("hello world", 5) != "hello..." {
		t.Error("long string should be truncated")
	}
}
