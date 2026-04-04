package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"
)

// ToolDef defines a tool the AI can call (Anthropic format).
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolCall represents a tool invocation from the AI response.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResult is sent back to the AI after executing a tool.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ContentBlock represents a single block in an Anthropic response content array.
type ContentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text,omitempty"`
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
}

// MultiMessage supports both simple string content and structured content blocks
// for the Anthropic multi-turn tool-use API.
type MultiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

// toolResponse is the raw Anthropic API response with content blocks.
type toolResponse struct {
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
}

// CallWithTools sends messages with tool definitions to the Anthropic/Vertex API
// and returns the text content and any tool calls from the response.
func CallWithTools(cfg Config, system string, messages []MultiMessage, tools []ToolDef, maxTokens int) (string, []ToolCall, string, error) {
	switch cfg.Provider {
	case ProviderVertex:
		return callVertexTools(cfg, system, messages, tools, maxTokens)
	case ProviderOpenAI:
		return "", nil, "", fmt.Errorf("tool-use not implemented for OpenAI provider")
	default:
		return callAnthropicTools(cfg, system, messages, tools, maxTokens)
	}
}

func callAnthropicTools(cfg Config, system string, messages []MultiMessage, tools []ToolDef, maxTokens int) (string, []ToolCall, string, error) {
	url := cfg.BaseURL + "/v1/messages"
	payload := map[string]any{
		"model":      cfg.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	if system != "" {
		payload["system"] = system
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	body, err := doAIRequest(url, payload, map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": "2023-06-01",
		"Content-Type":      "application/json",
	})
	if err != nil {
		return "", nil, "", err
	}
	return parseToolResponse(body)
}

func callVertexTools(cfg Config, system string, messages []MultiMessage, tools []ToolDef, maxTokens int) (string, []ToolCall, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", nil, "", fmt.Errorf("vertex ADC: %w", err)
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", nil, "", fmt.Errorf("vertex token: %w", err)
	}

	url := fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:rawPredict",
		cfg.Region, cfg.Project, cfg.Region, cfg.Model,
	)

	payload := map[string]any{
		"anthropic_version": "vertex-2023-10-16",
		"messages":          messages,
		"max_tokens":        maxTokens,
	}
	if system != "" {
		payload["system"] = system
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	body, err := doToolRequest(url, payload, map[string]string{
		"Authorization": "Bearer " + token.AccessToken,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return "", nil, "", err
	}
	return parseToolResponse(body)
}

// doToolRequest is like doAIRequest but with a longer timeout for agent loops.
func doToolRequest(url string, payload any, headers map[string]string) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AI tool request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return body, nil
}

// parseToolResponse extracts text and tool calls from an Anthropic content-block response.
func parseToolResponse(body []byte) (string, []ToolCall, string, error) {
	var resp toolResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", nil, "", fmt.Errorf("parse tool response: %w", err)
	}

	var text string
	var calls []ToolCall
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if text != "" {
				text += "\n"
			}
			text += block.Text
		case "tool_use":
			calls = append(calls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: block.Input,
			})
		}
	}
	return text, calls, resp.StopReason, nil
}

// RunAgentLoop runs a multi-turn conversation with tool use until the AI stops
// calling tools or maxIterations is reached.
// The toolHandler function receives a ToolCall and returns the result string.
func RunAgentLoop(cfg Config, system string, userMessage string, tools []ToolDef, toolHandler func(ToolCall) (string, bool), maxIterations int, maxTokens int) (string, int, int, error) {
	if !cfg.Available() {
		return "", 0, 0, fmt.Errorf("no AI provider configured")
	}

	messages := []MultiMessage{
		{Role: "user", Content: userMessage},
	}

	var finalText string
	totalToolCalls := 0

	for i := 0; i < maxIterations; i++ {
		slog.Info("agent loop iteration", "iteration", i+1, "messages", len(messages))

		text, calls, stopReason, err := CallWithTools(cfg, system, messages, tools, maxTokens)
		if err != nil {
			return finalText, i + 1, totalToolCalls, fmt.Errorf("iteration %d: %w", i+1, err)
		}

		finalText = text

		// No tool calls — AI is done
		if len(calls) == 0 || stopReason != "tool_use" {
			slog.Info("agent loop complete", "iterations", i+1, "tool_calls", totalToolCalls, "stop_reason", stopReason)
			return finalText, i + 1, totalToolCalls, nil
		}

		// Build assistant response with content blocks
		var assistantBlocks []ContentBlock
		if text != "" {
			assistantBlocks = append(assistantBlocks, ContentBlock{Type: "text", Text: text})
		}
		for _, call := range calls {
			assistantBlocks = append(assistantBlocks, ContentBlock{
				Type:  "tool_use",
				ID:    call.ID,
				Name:  call.Name,
				Input: call.Input,
			})
		}
		messages = append(messages, MultiMessage{Role: "assistant", Content: assistantBlocks})

		// Execute tools and collect results
		var toolResults []map[string]any
		for _, call := range calls {
			totalToolCalls++
			slog.Info("executing tool", "name", call.Name, "id", call.ID)
			result, isError := toolHandler(call)
			entry := map[string]any{
				"type":        "tool_result",
				"tool_use_id": call.ID,
				"content":     result,
			}
			if isError {
				entry["is_error"] = true
			}
			toolResults = append(toolResults, entry)
		}
		messages = append(messages, MultiMessage{Role: "user", Content: toolResults})
	}

	slog.Warn("agent loop hit max iterations", "max", maxIterations, "tool_calls", totalToolCalls)
	return finalText, maxIterations, totalToolCalls, nil
}

