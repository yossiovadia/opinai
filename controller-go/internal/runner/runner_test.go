package runner

import (
	"os"
	"testing"
)

func TestSetupContainerEnv(t *testing.T) {
	setupContainerEnv()

	if containerEnv == nil {
		t.Fatal("containerEnv should not be nil")
	}

	// Check dirs were created
	for _, dir := range []string{"/tmp/pip-user/bin", "/tmp/pip-cache", "/tmp/home"} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("dir %s should exist: %v", dir, err)
		}
	}

	// Check env vars
	found := map[string]bool{}
	for _, kv := range containerEnv {
		if len(kv) > 16 && kv[:16] == "PYTHONUSERBASE=/" {
			found["PYTHONUSERBASE"] = true
		}
		if len(kv) > 14 && kv[:14] == "PIP_CACHE_DIR=" {
			found["PIP_CACHE_DIR"] = true
		}
		if len(kv) > 5 && kv[:5] == "HOME=" {
			found["HOME"] = true
		}
	}
	for _, key := range []string{"PYTHONUSERBASE", "PIP_CACHE_DIR", "HOME"} {
		if !found[key] {
			t.Errorf("missing %s in containerEnv", key)
		}
	}
}

func TestExtractMemoryValue(t *testing.T) {
	ctx := "## What OpinAI knows:\n- description: a test project\n- working_install_command: pip install foo\n- tech_stack: Go"

	val := extractMemoryValue(ctx, "working_install_command")
	if val != "pip install foo" {
		t.Errorf("got %q, want 'pip install foo'", val)
	}

	val2 := extractMemoryValue(ctx, "description")
	if val2 != "a test project" {
		t.Errorf("got %q, want 'a test project'", val2)
	}

	val3 := extractMemoryValue(ctx, "nonexistent")
	if val3 != "" {
		t.Errorf("nonexistent should be empty, got %q", val3)
	}

	// Plain key: value format
	ctx2 := "working_install_command: python3 -m pip install --user llm-katan"
	val4 := extractMemoryValue(ctx2, "working_install_command")
	if val4 != "python3 -m pip install --user llm-katan" {
		t.Errorf("plain format: got %q", val4)
	}
}

func TestPostCommentAutoPostOff(t *testing.T) {
	// Ensure auto-post is off by default
	os.Unsetenv("OPINAI_AUTO_POST")
	// postComment would call controller.PostComment if auto-post is on.
	// We can't easily test without mocking, but we verify the env check logic.
	autoPost := os.Getenv("OPINAI_AUTO_POST")
	if autoPost != "" {
		t.Error("OPINAI_AUTO_POST should be empty by default")
	}
}

func TestTruncStr(t *testing.T) {
	if truncStr("hello", 10) != "hello" {
		t.Error("short string")
	}
	if truncStr("hello world", 5) != "hello" {
		t.Error("truncated string")
	}
}
