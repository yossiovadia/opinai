package ai

import (
	"encoding/json"
	"testing"
)

func TestParseToolResponse_TextOnly(t *testing.T) {
	body := `{
		"content": [{"type": "text", "text": "Hello, I will investigate this bug."}],
		"stop_reason": "end_turn"
	}`
	text, calls, stopReason, err := parseToolResponse([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Hello, I will investigate this bug." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(calls))
	}
	if stopReason != "end_turn" {
		t.Errorf("stop_reason = %q", stopReason)
	}
}

func TestParseToolResponse_ToolUse(t *testing.T) {
	body := `{
		"content": [
			{"type": "text", "text": "Let me check the code."},
			{"type": "tool_use", "id": "toolu_abc123", "name": "read_file", "input": {"path": "main.go"}}
		],
		"stop_reason": "tool_use"
	}`
	text, calls, stopReason, err := parseToolResponse([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Let me check the code." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "toolu_abc123" {
		t.Errorf("call ID = %q", calls[0].ID)
	}
	if calls[0].Name != "read_file" {
		t.Errorf("call name = %q", calls[0].Name)
	}
	path, _ := calls[0].Input["path"].(string)
	if path != "main.go" {
		t.Errorf("call input path = %q", path)
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q", stopReason)
	}
}

func TestParseToolResponse_MultipleToolCalls(t *testing.T) {
	body := `{
		"content": [
			{"type": "tool_use", "id": "toolu_1", "name": "grep", "input": {"pattern": "stream", "path": "."}},
			{"type": "tool_use", "id": "toolu_2", "name": "list_dir", "input": {"path": "src"}}
		],
		"stop_reason": "tool_use"
	}`
	text, calls, _, err := parseToolResponse([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].Name != "grep" || calls[1].Name != "list_dir" {
		t.Errorf("unexpected tool names: %q, %q", calls[0].Name, calls[1].Name)
	}
}

func TestParseToolResponse_InvalidJSON(t *testing.T) {
	_, _, _, err := parseToolResponse([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseToolResponse_EmptyContent(t *testing.T) {
	body := `{"content": [], "stop_reason": "end_turn"}`
	text, calls, _, err := parseToolResponse([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "" {
		t.Errorf("expected empty text, got %q", text)
	}
	if len(calls) != 0 {
		t.Errorf("expected no calls, got %d", len(calls))
	}
}

func TestMultiMessage_JSONMarshal(t *testing.T) {
	// String content
	msg := MultiMessage{Role: "user", Content: "hello"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal string content: %v", err)
	}
	if string(data) != `{"role":"user","content":"hello"}` {
		t.Errorf("string content JSON = %s", data)
	}

	// Content blocks (assistant with tool_use)
	blocks := []ContentBlock{
		{Type: "text", Text: "checking..."},
		{Type: "tool_use", ID: "toolu_1", Name: "grep", Input: map[string]any{"pattern": "bug"}},
	}
	msg2 := MultiMessage{Role: "assistant", Content: blocks}
	data2, err := json.Marshal(msg2)
	if err != nil {
		t.Fatalf("marshal blocks content: %v", err)
	}
	// Verify it round-trips
	var parsed map[string]any
	if err := json.Unmarshal(data2, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["role"] != "assistant" {
		t.Errorf("role = %v", parsed["role"])
	}
	contentArr, ok := parsed["content"].([]any)
	if !ok || len(contentArr) != 2 {
		t.Errorf("expected 2 content blocks, got %v", parsed["content"])
	}

	// Tool result content (user message with tool_result blocks)
	results := []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": "found 5 matches"},
	}
	msg3 := MultiMessage{Role: "user", Content: results}
	data3, err := json.Marshal(msg3)
	if err != nil {
		t.Fatalf("marshal tool results: %v", err)
	}
	var parsed3 map[string]any
	if err := json.Unmarshal(data3, &parsed3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed3["role"] != "user" {
		t.Errorf("role = %v", parsed3["role"])
	}
}

func TestToolDefJSON(t *testing.T) {
	tool := ToolDef{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
	}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool def: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["name"] != "read_file" {
		t.Errorf("name = %v", parsed["name"])
	}
	schema, ok := parsed["input_schema"].(map[string]any)
	if !ok {
		t.Fatal("input_schema not a map")
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v", schema["type"])
	}
}
