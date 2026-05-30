// Package analysis calls an OpenAI chat-completions compatible endpoint to
// generate the executive narrative that opens each benchmark PDF. The endpoint
// (and the model name) are passed in by the caller — both are typically read
// from environment variables. We use a hand-rolled minimal client instead of
// pulling the full openai-go SDK.
package analysis

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/helmcode/nan-benchmarks-agent/internal/report"
)

//go:embed examples/example1.md
var example1 string

//go:embed examples/example2.md
var example2 string

// Client holds configuration for the LLM call.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// New returns a Client. baseURL and model are both required — empty values
// fall back to placeholders that will fail at request time, surfacing a
// configuration error early instead of silently calling a default endpoint.
func New(baseURL, apiKey, model string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:0"
	}
	if model == "" {
		model = "unset"
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Analyze posts the report to the LLM and returns the markdown narrative.
func (c *Client) Analyze(ctx context.Context, r *report.Report) (string, error) {
	user, err := buildUserMessage(r)
	if err != nil {
		return "", fmt.Errorf("build user message: %w", err)
	}

	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt()},
			{Role: "user", Content: user},
		},
		Temperature: 0.3,
		MaxTokens:   4096,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api status %d: %s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("api error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("api returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

func buildUserMessage(r *report.Report) (string, error) {
	payload, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Mode: %s\nWindow: %s ending %s UTC\nPrevious window ended: %s UTC\n\n"+
			"Dataset:\n```json\n%s\n```\n\n"+
			"Write the executive analysis (Executive Summary, Key Findings, "+
			"Trade-offs / Risks, Capacity Planning, Recommendations) following "+
			"the style of the examples in the system prompt. Output markdown only, "+
			"no preamble.",
		r.Mode, r.Window,
		r.PeriodEnd.UTC().Format(time.RFC3339),
		r.PrevEnd.UTC().Format(time.RFC3339),
		payload,
	), nil
}
