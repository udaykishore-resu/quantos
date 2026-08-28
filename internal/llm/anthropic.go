package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic is an adapter for the Anthropic Messages API.
//
// It is written directly against the documented HTTP contract rather than a
// vendor SDK, which keeps the dependency surface of a financial platform small
// and makes the request and response shapes visible at the call site. The API
// key is read from the environment by the caller and never stored in
// configuration, never logged and never sent to the frontend.
type Anthropic struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
	version string
}

// AnthropicConfig parameterises the adapter.
type AnthropicConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	Timeout time.Duration
}

// NewAnthropic builds an adapter. An empty API key yields a provider that fails
// fast with ErrNoAPIKey, so a misconfigured deployment degrades to the
// deterministic template renderer rather than hanging.
func NewAnthropic(cfg AnthropicConfig) *Anthropic {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-5"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	return &Anthropic{
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  &http.Client{Timeout: cfg.Timeout},
		version: "2023-06-01",
	}
}

// Name implements Provider.
func (a *Anthropic) Name() string { return "anthropic" }

// Model implements Provider.
func (a *Anthropic) Model() string { return a.model }

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature,omitempty"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete calls the Messages API.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if a.apiKey == "" {
		return Response{}, ErrNoAPIKey
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	msgs := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, anthropicMessage{Role: string(m.Role), Content: m.Content})
	}
	body, err := json.Marshal(anthropicRequest{
		Model: a.model, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		System: req.System, Messages: msgs,
	})
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", a.version)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return Response{}, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: reading response: %v", ErrUnavailable, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return Response{}, ErrRateLimited
	case resp.StatusCode >= 500:
		return Response{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	case resp.StatusCode >= 400:
		var ar anthropicResponse
		_ = json.Unmarshal(raw, &ar)
		msg := "unknown error"
		if ar.Error != nil {
			msg = ar.Error.Message
		}
		return Response{}, fmt.Errorf("llm: status %d: %s", resp.StatusCode, msg)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	var text strings.Builder
	for _, c := range ar.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return Response{
		Text:         text.String(),
		Model:        ar.Model,
		Provider:     a.Name(),
		InputTokens:  ar.Usage.InputTokens,
		OutputTokens: ar.Usage.OutputTokens,
		Latency:      time.Since(start),
		StopReason:   ar.StopReason,
	}, nil
}
