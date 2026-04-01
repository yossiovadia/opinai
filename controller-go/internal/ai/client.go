// Package ai provides a multi-provider AI client (Anthropic, OpenAI, Vertex AI).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// Provider determines which AI API to call.
type Provider int

const (
	ProviderAnthropic Provider = iota
	ProviderOpenAI
	ProviderVertex
)

// Config holds AI client configuration from env vars.
type Config struct {
	Provider Provider
	APIKey   string
	Model    string
	BaseURL  string
	Project  string // Vertex AI
	Region   string // Vertex AI
}

// LoadConfig reads AI config from environment variables.
func LoadConfig() Config {
	c := Config{
		APIKey:  os.Getenv("AI_API_KEY"),
		Model:   envOr("AI_MODEL", "claude-sonnet-4-20250514"),
		BaseURL: envOr("AI_BASE_URL", "https://api.anthropic.com"),
		Project: os.Getenv("AI_PROJECT"),
		Region:  os.Getenv("AI_REGION"),
	}
	switch strings.ToLower(os.Getenv("AI_PROVIDER")) {
	case "vertex":
		c.Provider = ProviderVertex
	case "openai":
		c.Provider = ProviderOpenAI
	default:
		if strings.Contains(strings.ToLower(c.BaseURL), "openai") {
			c.Provider = ProviderOpenAI
		} else {
			c.Provider = ProviderAnthropic
		}
	}
	return c
}

// Available returns true if an AI provider is configured.
func (c Config) Available() bool {
	if c.Provider == ProviderVertex {
		return c.Project != "" && c.Region != ""
	}
	return c.APIKey != ""
}

// Message is a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Call sends a prompt to the AI and returns the text response.
func Call(prompt string, maxTokens int) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "", fmt.Errorf("no AI provider configured")
	}
	return callWithConfig(cfg, prompt, maxTokens)
}

// CallWithConfig sends a prompt using a specific config.
func callWithConfig(cfg Config, prompt string, maxTokens int) (string, error) {
	messages := []Message{{Role: "user", Content: prompt}}

	switch cfg.Provider {
	case ProviderVertex:
		return callVertex(cfg, messages, maxTokens)
	case ProviderOpenAI:
		return callOpenAI(cfg, messages, maxTokens)
	default:
		return callAnthropic(cfg, messages, maxTokens)
	}
}

func callVertex(cfg Config, messages []Message, maxTokens int) (string, error) {
	// Get ADC access token
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("vertex ADC: %w", err)
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("vertex token: %w", err)
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

	body, err := doAIRequest(url, payload, map[string]string{
		"Authorization": "Bearer " + token.AccessToken,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return "", err
	}
	return extractAnthropicText(body)
}

func callAnthropic(cfg Config, messages []Message, maxTokens int) (string, error) {
	url := cfg.BaseURL + "/v1/messages"
	payload := map[string]any{
		"model":      cfg.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	body, err := doAIRequest(url, payload, map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": "2023-06-01",
		"Content-Type":      "application/json",
	})
	if err != nil {
		return "", err
	}
	return extractAnthropicText(body)
}

func callOpenAI(cfg Config, messages []Message, maxTokens int) (string, error) {
	url := cfg.BaseURL + "/v1/chat/completions"
	payload := map[string]any{
		"model":      cfg.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	body, err := doAIRequest(url, payload, map[string]string{
		"Authorization": "Bearer " + cfg.APIKey,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return "", err
	}
	return extractOpenAIText(body)
}

func doAIRequest(url string, payload any, headers map[string]string) ([]byte, error) {
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

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AI request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func extractAnthropicText(body []byte) (string, error) {
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse AI response: %w", err)
	}
	if len(resp.Content) == 0 {
		return "", fmt.Errorf("empty AI response")
	}
	return resp.Content[0].Text, nil
}

func extractOpenAIText(body []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse AI response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty AI response")
	}
	return resp.Choices[0].Message.Content, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Sanitize removes credentials from text.
func Sanitize(text string) string {
	for _, key := range []string{"AI_API_KEY", "GITHUB_TOKEN"} {
		secret := os.Getenv(key)
		if len(secret) > 8 {
			text = strings.ReplaceAll(text, secret, "REDACTED")
		}
	}
	return text
}

func init() {
	_ = slog.Default() // suppress unused import
}
