package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/infrastructure/openai"
)

// ---------- helpers ----------

func newTestClient(t *testing.T, h http.HandlerFunc) (*openai.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := openai.NewClient(openai.Config{
		Deployment:   openai.DeploymentOpenAI,
		BaseURL:      srv.URL,
		APIKey:       "test-key",
		DefaultModel: "gpt-4o-mini",
		ModelByRole: map[llm.ModelRole]string{
			llm.RoleRisk:         "gpt-4o-mini",
			llm.RoleReleaseNotes: "gpt-4o",
		},
		Timeout:    2 * time.Second,
		MaxRetries: 2,
	})
	require.NoError(t, err)
	return c, srv
}

func okBody(model, content string) string {
	body := map[string]any{
		"id":    "chatcmpl-test",
		"model": model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{
			"prompt_tokens":     11,
			"completion_tokens": 22,
			"total_tokens":      33,
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// ---------- config validation ----------

func TestNewClient_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  openai.Config
	}{
		{"empty base url", openai.Config{APIKey: "k", DefaultModel: "m"}},
		{"non-http base url", openai.Config{BaseURL: "ftp://x", APIKey: "k", DefaultModel: "m"}},
		{"empty api key", openai.Config{BaseURL: "https://x", DefaultModel: "m"}},
		{"empty default model", openai.Config{BaseURL: "https://x", APIKey: "k"}},
		{"unknown deployment", openai.Config{Deployment: "unknown", BaseURL: "https://x", APIKey: "k", DefaultModel: "m"}},
		{"negative retries", openai.Config{BaseURL: "https://x", APIKey: "k", DefaultModel: "m", MaxRetries: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := openai.NewClient(tc.cfg)
			require.Error(t, err)
		})
	}
}

// ---------- happy path ----------

func TestComplete_HappyPath(t *testing.T) {
	var captured chatRequestCapture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &captured))

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", `{"ok":true}`))
	})

	temp := 0.2
	resp, err := c.Complete(context.Background(), llm.Request{
		ModelRole: llm.RoleRisk,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are a risk analyzer."},
			{Role: llm.RoleUser, Content: "Analyze this PR."},
		},
		Temperature:     temp,
		MaxOutputTokens: 256,
	})
	require.NoError(t, err)
	require.Equal(t, `{"ok":true}`, resp.Content)
	require.Equal(t, "gpt-4o-mini", resp.Model)
	require.Equal(t, "stop", resp.FinishReason)
	require.Equal(t, 11, resp.Usage.PromptTokens)
	require.Equal(t, 22, resp.Usage.CompletionTokens)

	// Wire request sanity.
	require.Equal(t, "gpt-4o-mini", captured.Model)
	require.Len(t, captured.Messages, 2)
	require.Equal(t, "system", captured.Messages[0].Role)
	require.Equal(t, 256, captured.MaxTokens)
	require.NotNil(t, captured.Temperature)
	require.InDelta(t, 0.2, *captured.Temperature, 0.0001)
}

func TestComplete_ResolvesModelByRole(t *testing.T) {
	var capturedModel atomic.Value
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var cap chatRequestCapture
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &cap)
		capturedModel.Store(cap.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody(cap.Model, "ok"))
	})

	_, err := c.Complete(context.Background(), llm.Request{
		ModelRole: llm.RoleReleaseNotes,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	require.Equal(t, "gpt-4o", capturedModel.Load())
}

func TestComplete_StructuredOutput(t *testing.T) {
	var captured chatRequestCapture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &captured))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", `{"Level":3,"Factors":[],"Explanation":"x","Confidence":0.7}`))
	})

	spec, ok := openai.SchemaFor(analysis.KindRisk)
	require.True(t, ok)

	resp, err := c.Complete(context.Background(), llm.Request{
		Messages:         []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		StructuredOutput: spec,
	})
	require.NoError(t, err)
	require.Contains(t, resp.Content, `"Level":3`)
	require.NotNil(t, captured.ResponseFormat)
	require.Equal(t, "json_schema", captured.ResponseFormat.Type)
	require.NotNil(t, captured.ResponseFormat.JSONSchema)
	require.Equal(t, "risk_report", captured.ResponseFormat.JSONSchema.Name)
	require.True(t, captured.ResponseFormat.JSONSchema.Strict)
	require.True(t, json.Valid(captured.ResponseFormat.JSONSchema.Schema))
}

// ---------- error classification ----------

func TestComplete_Unauthorized(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
	})
	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.ErrorIs(t, err, llm.ErrUnauthorized)
}

func TestComplete_ModelUnavailable(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"The model 'gpt-9' does not exist","code":"model_not_found"}}`)
	})
	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.ErrorIs(t, err, llm.ErrModelUnavailable)
}

func TestComplete_ContentFiltered(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"id":    "x",
			"model": "gpt-4o-mini",
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "content_filter",
				"message":       map[string]any{"role": "assistant", "content": ""},
			}},
		}
		b, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.ErrorIs(t, err, llm.ErrContentFiltered)
}

// ---------- retries ----------

func TestComplete_RetriesOn429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", "ok"))
	})

	resp, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)
	require.EqualValues(t, 2, hits.Load())
}

func TestComplete_GivesUpAfterMaxRetries(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})

	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "server error 500"),
		"expected server error, got %v", err)
	require.EqualValues(t, 3, hits.Load(), "should hit MaxRetries+1 times")
}

func TestComplete_RetriesOn500ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"transient"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", "after-retry"))
	})

	resp, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	require.Equal(t, "after-retry", resp.Content)
	require.EqualValues(t, 2, hits.Load())
}

// ---------- malformed-200 responses ----------

func TestComplete_MalformedJSONResponse(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `not-json-at-all`)
	})
	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestComplete_NoChoicesInResponse(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"gpt-4o-mini","choices":[]}`)
	})
	_, err := c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, llm.ErrInvalidResponse)
}

func TestComplete_RespectsContextCancel(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", "ok"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

// ---------- Azure & GitHub Models specifics ----------

func TestAzureDeployment_UsesAPIKeyHeaderAndVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "test-key", r.Header.Get("api-key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "2024-10-21", r.URL.Query().Get("api-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", "ok"))
	}))
	t.Cleanup(srv.Close)

	c, err := openai.NewClient(openai.Config{
		Deployment:   openai.DeploymentAzureOpenAI,
		BaseURL:      srv.URL,
		APIKey:       "test-key",
		DefaultModel: "gpt-4o-mini",
	})
	require.NoError(t, err)
	_, err = c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
}

func TestGitHubModelsDeployment_UsesBearerHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("api-key"))
		require.Empty(t, r.URL.Query().Get("api-version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, okBody("gpt-4o-mini", "ok"))
	}))
	t.Cleanup(srv.Close)

	c, err := openai.NewClient(openai.Config{
		Deployment:   openai.DeploymentGitHubModels,
		BaseURL:      srv.URL,
		APIKey:       "test-key",
		DefaultModel: "gpt-4o-mini",
	})
	require.NoError(t, err)
	_, err = c.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
}

// ---------- schema accessors ----------

func TestSchemaFor_AllDomainKinds(t *testing.T) {
	for _, k := range []analysis.Kind{
		analysis.KindRisk,
		analysis.KindReleaseNotes,
		analysis.KindQARecommendations,
	} {
		spec, ok := openai.SchemaFor(k)
		require.True(t, ok, "kind %s", k)
		require.NotEmpty(t, spec.Name)
		require.True(t, json.Valid(spec.Schema), "schema for %s is not valid json", k)
		require.True(t, spec.Strict)
	}
	spec, ok := openai.SchemaFor(analysis.Kind("unknown"))
	require.False(t, ok)
	require.Nil(t, spec)
}

// ---------- private wire-capture type ----------

// chatRequestCapture mirrors the wire types in dto.go for assertions.
// Kept here (test-only) so we don't have to export the production
// types.
type chatRequestCapture struct {
	Model          string                 `json:"model"`
	Messages       []chatMessageCapture   `json:"messages"`
	Temperature    *float64               `json:"temperature,omitempty"`
	MaxTokens      int                    `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormatCapture `json:"response_format,omitempty"`
}
type chatMessageCapture struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type responseFormatCapture struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema,omitempty"`
}
