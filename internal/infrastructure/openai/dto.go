package openai

import "encoding/json"

// ---------- Request wire types ----------

// chatRequest is the wire shape sent to POST /chat/completions.
// Fields with omitempty stay absent when callers don't set them,
// preserving provider-side defaults.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat carries either a json_object request (loose) or a
// json_schema request (strict). The provider's behaviour is the
// difference between "the model emits SOME json" and "the model emits
// json matching this exact schema or the call fails".
type responseFormat struct {
	Type       string             `json:"type"` // "json_object" | "json_schema"
	JSONSchema *jsonSchemaWrapper `json:"json_schema,omitempty"`
}

type jsonSchemaWrapper struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema"`
}

// ---------- Response wire types ----------

// chatResponse is the wire shape returned by /chat/completions.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---------- Error wire types ----------

// errorEnvelope is the shape returned by every error response from
// OpenAI-compatible providers. Some providers omit fields; we tolerate
// that.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}
