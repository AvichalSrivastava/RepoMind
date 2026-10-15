package openai

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

	"github.com/repomind/repomind/internal/application/llm"
)

const (
	defaultTimeout      = 60 * time.Second
	defaultMaxRetries   = 3
	defaultUserAgent    = "RepoMind/1.0"
	defaultAzureVersion = "2024-10-21"
	maxBackoff          = 10 * time.Second
	bodySnippetMaxBytes = 1024
)

// Compile-time guarantee that *Client satisfies the application port.
var _ llm.Provider = (*Client)(nil)

// Client is the concrete OpenAI-compatible LLM client.
type Client struct {
	cfg  Config
	http *http.Client
	dep  Deployment
}

// NewClient validates cfg and constructs a Client.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	dep := cfg.effectiveDeployment()
	if dep == DeploymentAzureOpenAI && cfg.AzureAPIVersion == "" {
		cfg.AzureAPIVersion = defaultAzureVersion
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}, dep: dep}, nil
}

// Complete implements llm.Provider.
func (c *Client) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("openai: at least one message is required")
	}

	wire := chatRequest{
		Model:    c.cfg.resolveModel(req.ModelRole),
		Messages: toWireMessages(req.Messages),
	}
	if req.MaxOutputTokens > 0 {
		wire.MaxTokens = req.MaxOutputTokens
	}
	if req.Temperature >= 0 {
		t := req.Temperature
		wire.Temperature = &t
	}
	if req.StructuredOutput != nil {
		rf, err := toResponseFormat(req.StructuredOutput)
		if err != nil {
			return nil, err
		}
		wire.ResponseFormat = rf
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	endpoint, err := c.completionsEndpoint(wire.Model)
	if err != nil {
		return nil, err
	}

	var lastErr error
	attempts := c.cfg.MaxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt, lastErr); err != nil {
				return nil, err
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setAuth(httpReq)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", c.cfg.UserAgent)

		resp, err := c.http.Do(httpReq)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			lastErr = err
			continue
		}

		out, decodeErr := decodeResponse(resp)
		_ = resp.Body.Close()
		if decodeErr == nil {
			return out, nil
		}
		lastErr = decodeErr
		if !isRetryable(decodeErr) {
			return nil, decodeErr
		}
	}
	return nil, lastErr
}

// ---------- Endpoint resolution ----------

// completionsEndpoint computes the URL for the chat-completions call,
// accounting for the per-deployment differences:
//
//   - openai / github_models: <base>/chat/completions
//   - azure_openai:           <base>/chat/completions?api-version=<v>
//     (the base already encodes the deployment name)
func (c *Client) completionsEndpoint(_ string) (string, error) {
	switch c.dep {
	case DeploymentAzureOpenAI:
		sep := "?"
		if strings.Contains(c.cfg.BaseURL, "?") {
			sep = "&"
		}
		return c.cfg.BaseURL + "/chat/completions" + sep + "api-version=" + c.cfg.AzureAPIVersion, nil
	default:
		return c.cfg.BaseURL + "/chat/completions", nil
	}
}

// setAuth applies the correct authentication header for the
// configured deployment.
func (c *Client) setAuth(req *http.Request) {
	if c.dep == DeploymentAzureOpenAI {
		req.Header.Set("api-key", c.cfg.APIKey)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
}

// ---------- Request translation ----------

func toWireMessages(in []llm.Message) []chatMessage {
	out := make([]chatMessage, 0, len(in))
	for _, m := range in {
		role := string(m.Role)
		if role == "" {
			role = string(llm.RoleUser)
		}
		out = append(out, chatMessage{Role: role, Content: m.Content})
	}
	return out
}

func toResponseFormat(s *llm.StructuredOutputSpec) (*responseFormat, error) {
	if s == nil {
		return nil, nil
	}
	if strings.TrimSpace(s.Name) == "" {
		return nil, fmt.Errorf("openai: structured output name is required")
	}
	if len(s.Schema) == 0 {
		return nil, fmt.Errorf("openai: structured output schema is required")
	}
	if !json.Valid(s.Schema) {
		return nil, fmt.Errorf("openai: structured output schema is not valid JSON")
	}
	return &responseFormat{
		Type: "json_schema",
		JSONSchema: &jsonSchemaWrapper{
			Name:   s.Name,
			Strict: s.Strict,
			Schema: json.RawMessage(s.Schema),
		},
	}, nil
}

// ---------- Response decoding ----------

func decodeResponse(resp *http.Response) (*llm.Response, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("openai: read body: %w", readErr)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTPError(resp, body)
	}

	var wire chatResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%w: malformed json: %v", llm.ErrInvalidResponse, err)
	}
	if len(wire.Choices) == 0 {
		return nil, fmt.Errorf("%w: response has no choices", llm.ErrInvalidResponse)
	}

	choice := wire.Choices[0]
	if choice.FinishReason == "content_filter" {
		return nil, llm.ErrContentFiltered
	}

	return &llm.Response{
		Content:      choice.Message.Content,
		Model:        wire.Model,
		FinishReason: choice.FinishReason,
		Usage: llm.Usage{
			PromptTokens:     wire.Usage.PromptTokens,
			CompletionTokens: wire.Usage.CompletionTokens,
			TotalTokens:      wire.Usage.TotalTokens,
		},
	}, nil
}

// classifyHTTPError maps an HTTP error response to one of the
// application-layer sentinels.
func classifyHTTPError(resp *http.Response, body []byte) error {
	var env errorEnvelope
	_ = json.Unmarshal(body, &env) // best-effort
	msg := env.Error.Message
	if msg == "" {
		msg = snippet(body)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", llm.ErrUnauthorized, msg)
	case resp.StatusCode == http.StatusTooManyRequests:
		return &rateLimitErr{
			msg: msg,
			ra:  parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "model"):
		return fmt.Errorf("%w: %s", llm.ErrModelUnavailable, msg)
	case resp.StatusCode >= 500:
		return &serverErr{status: resp.StatusCode, msg: msg}
	case resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: %s", llm.ErrInvalidResponse, msg)
	default:
		return fmt.Errorf("openai: unexpected status %d: %s", resp.StatusCode, msg)
	}
}

// ---------- Retry helpers ----------

// isRetryable returns true for transient errors that the caller
// should retry. Non-retryable sentinels win first; everything that
// satisfies the package-private retryable interface (rateLimitErr,
// serverErr) is retried.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, llm.ErrUnauthorized) ||
		errors.Is(err, llm.ErrInvalidResponse) ||
		errors.Is(err, llm.ErrModelUnavailable) ||
		errors.Is(err, llm.ErrContentFiltered) {
		return false
	}
	var r retryable
	return errors.As(err, &r)
}

// sleepBackoff applies an exponential backoff capped by maxBackoff.
// If the most recent error carries a server-suggested wait (a 429
// with Retry-After), that wait wins over the computed backoff.
// Context cancellation aborts immediately.
func sleepBackoff(ctx context.Context, attempt int, lastErr error) error {
	d := time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
	if d > maxBackoff {
		d = maxBackoff
	}
	var r retryable
	if errors.As(lastErr, &r) {
		if ra := r.retryAfter(); ra > 0 {
			d = ra
			if d > maxBackoff {
				d = maxBackoff
			}
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// snippet returns a bounded, single-line preview of an opaque body for
// error messages.
func snippet(b []byte) string {
	if len(b) > bodySnippetMaxBytes {
		b = b[:bodySnippetMaxBytes]
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\n", " "))
}
