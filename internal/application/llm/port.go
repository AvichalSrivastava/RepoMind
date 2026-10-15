// Package llm defines the application-layer port that analyzers use to
// invoke a large-language-model provider. The infrastructure adapter
// (internal/infrastructure/openai) speaks the OpenAI Chat Completions
// wire protocol, which is supported by OpenAI, Azure OpenAI, and
// GitHub Models — switching among the three is a config change, not a
// code change.
//
// Nothing in the domain layer imports this package. Application
// services (Modules 5-7) depend on Provider; the composition root in
// cmd/repomind wires the concrete adapter.
package llm

import (
	"context"
	"errors"
)

// ---------- Provider port ----------

// Provider is the seam between RepoMind's analyzers and any
// OpenAI-protocol-compatible LLM backend. Implementations MUST be safe
// for concurrent use.
type Provider interface {
	// Complete runs a chat completion. The response payload is the
	// raw JSON bytes returned by the model when StructuredOutput is
	// requested, or the raw assistant message text otherwise.
	//
	// Implementations MUST honour ctx for cancellation and deadline,
	// retry transient failures (429 / 5xx) up to a bounded number of
	// attempts, and translate provider-side errors into the sentinels
	// declared below.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// ---------- Request / response ----------

// Role identifies a chat message author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one chat turn.
type Message struct {
	Role    Role
	Content string
}

// Request is the analyzer-facing call shape. It is intentionally
// narrow — only what analyzers actually need today.
type Request struct {
	// ModelRole is the semantic role of this call (RoleRisk,
	// RoleReleaseNotes, RoleQA, …). The adapter resolves it to a
	// concrete model string from its config. Empty means use the
	// adapter's default model.
	ModelRole ModelRole

	// Messages is the chat history in send order. Typically: one
	// system message + one user message.
	Messages []Message

	// MaxOutputTokens caps the response length. Zero leaves the
	// provider default in place.
	MaxOutputTokens int

	// Temperature controls sampling randomness, 0..2. Values < 0 are
	// treated as "use provider default".
	Temperature float64

	// StructuredOutput, when non-nil, asks the provider to return a
	// JSON object matching the schema. The adapter sets
	// response_format = json_schema with strict mode where the
	// provider supports it.
	StructuredOutput *StructuredOutputSpec
}

// StructuredOutputSpec carries a JSON Schema that constrains the
// model's reply to a single JSON object the analyzer can unmarshal
// directly into a typed payload.
//
// The Schema field is the raw JSON Schema document as bytes — keeping
// it as bytes lets adapters pass it through to the provider without a
// round-trip through map[string]any, and lets callers embed schemas
// from //go:embed files.
type StructuredOutputSpec struct {
	// Name identifies the schema in the provider's API call (e.g.
	// "risk_report"). The provider returns it back so the caller can
	// route the response to the right unmarshal target.
	Name string

	// Schema is the JSON Schema document (RFC 8927 / OpenAPI subset
	// accepted by the provider).
	Schema []byte

	// Strict asks the provider to refuse non-conforming output. Set
	// to true for production callers; the adapter silently downgrades
	// when the configured provider doesn't support strict mode.
	Strict bool
}

// Response is what the adapter returns to the analyzer.
type Response struct {
	// Content is the raw assistant text. For structured-output
	// requests this is the JSON object as a string, suitable for
	// json.Unmarshal into the analyzer's payload type.
	Content string

	// Model is the actual model the provider used (echoes the
	// resolved model string, e.g. "gpt-4o-mini-2024-07-18").
	Model string

	// Usage carries the provider's token accounting. The analyzer
	// stores these on the AnalysisResult aggregate.
	Usage Usage

	// FinishReason is the provider's stop reason
	// ("stop" | "length" | "content_filter" | …). The analyzer logs
	// it but is generally not expected to branch on it.
	FinishReason string
}

// Usage is the provider's token accounting for a single call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ---------- Model role routing ----------

// ModelRole is the analyzer-side semantic role for an LLM call. The
// adapter maps it to a concrete model name via config so analyzers
// don't hard-code model identifiers.
type ModelRole string

const (
	// RoleDefault is the fallback when an analyzer doesn't ask for a
	// specific role.
	RoleDefault ModelRole = ""

	// RoleRisk is used by the risk analyzer (Module 5). Tends to be a
	// fast / cheap classification model.
	RoleRisk ModelRole = "risk"

	// RoleReleaseNotes is used by the release-notes composer
	// (Module 7). Prose quality matters; usually a flagship model.
	RoleReleaseNotes ModelRole = "release_notes"

	// RoleQA is used by the QA recommendation analyzer (Module 6).
	// Typically a fast model.
	RoleQA ModelRole = "qa"
)

// ---------- Sentinel errors ----------

var (
	// ErrRateLimited indicates the provider returned 429 after the
	// adapter exhausted its retry budget. Callers may treat this as
	// transient.
	ErrRateLimited = errors.New("llm: rate limited")

	// ErrModelUnavailable indicates the requested model is unknown to
	// the provider or has been retired. Not retryable.
	ErrModelUnavailable = errors.New("llm: model unavailable")

	// ErrInvalidResponse indicates the provider returned a response
	// the adapter could not parse (malformed JSON, missing choices,
	// schema-strict violation, …). Not retryable.
	ErrInvalidResponse = errors.New("llm: invalid response")

	// ErrUnauthorized indicates 401/403 from the provider. Not
	// retryable; almost always a misconfigured API key.
	ErrUnauthorized = errors.New("llm: unauthorized")

	// ErrContentFiltered indicates the provider's safety filter
	// blocked the request or response. Not retryable.
	ErrContentFiltered = errors.New("llm: content filtered")
)
