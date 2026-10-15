// Package openai is the infrastructure adapter that implements
// internal/application/llm.Provider against the OpenAI Chat
// Completions protocol. The same protocol is spoken by OpenAI itself,
// Azure OpenAI, and GitHub Models — pointing the client at any of the
// three is purely a BaseURL + APIKey + model-name change.
//
// The adapter is hand-rolled (no SDK) for the same reasons the Jira
// adapter is: we need exactly one endpoint, full control over retries
// and error classification, and a tiny dependency surface.
package openai

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/application/llm"
)

// Deployment selects defaults appropriate for one of the three
// supported deployment targets. The wire protocol is identical across
// all three; only base URL and auth header style differ in practice.
type Deployment string

const (
	// DeploymentOpenAI targets api.openai.com directly.
	DeploymentOpenAI Deployment = "openai"

	// DeploymentGitHubModels targets GitHub Models
	// (models.github.ai). Free for personal accounts with rate
	// limits; uses a GitHub PAT as the API key.
	DeploymentGitHubModels Deployment = "github_models"

	// DeploymentAzureOpenAI targets an Azure OpenAI resource. The
	// caller is responsible for putting the deployment name into the
	// per-role model map (Azure uses deployment names where OpenAI
	// uses model names).
	DeploymentAzureOpenAI Deployment = "azure_openai"
)

// Config controls the OpenAI-compatible HTTP client. Build it from
// infrastructure/config.Config.OpenAI at the composition root.
type Config struct {
	// Deployment selects target-specific defaults. Optional — when
	// empty the adapter assumes "openai".
	Deployment Deployment

	// BaseURL is the API root. Examples:
	//   https://api.openai.com/v1
	//   https://models.github.ai/inference
	//   https://<resource>.openai.azure.com/openai/deployments/<deployment>
	BaseURL string

	// APIKey authenticates every request. Sent as
	//   Authorization: Bearer <key>          (OpenAI, GitHub Models)
	//   api-key: <key>                       (Azure OpenAI)
	APIKey string

	// AzureAPIVersion is appended as ?api-version=... when Deployment
	// is azure_openai. Ignored otherwise. Defaults to a recent stable
	// version when empty.
	AzureAPIVersion string

	// DefaultModel is used when an analyzer's Request specifies no
	// ModelRole (or the role is not present in ModelByRole).
	DefaultModel string

	// ModelByRole maps semantic roles to concrete model identifiers.
	// Lets analyzers ask for "release_notes quality" while the
	// composition root decides whether that is gpt-4o, claude-opus,
	// llama-3-405b, etc.
	ModelByRole map[llm.ModelRole]string

	// Timeout is the per-request HTTP timeout. Defaults to 60s
	// (chat completions are noticeably slower than typical REST).
	Timeout time.Duration

	// MaxRetries bounds retry attempts on 429 / 5xx. Defaults to 3.
	MaxRetries int

	// UserAgent overrides the User-Agent header. Defaults to
	// "RepoMind/1.0".
	UserAgent string
}

// validate checks the Config for internal consistency. It does not
// mutate the receiver; defaults are applied inside NewClient.
func (c Config) validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("openai: base_url is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("openai: base_url must be a valid http(s) URL")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("openai: api_key is required")
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		return errors.New("openai: default_model is required")
	}
	switch c.Deployment {
	case "", DeploymentOpenAI, DeploymentGitHubModels, DeploymentAzureOpenAI:
		// ok
	default:
		return fmt.Errorf("openai: unknown deployment %q", c.Deployment)
	}
	if c.MaxRetries < 0 {
		return errors.New("openai: max_retries must be >= 0")
	}
	return nil
}

// resolveModel returns the concrete model name for a given role,
// falling back to DefaultModel when the role is unset or unmapped.
func (c Config) resolveModel(role llm.ModelRole) string {
	if role != llm.RoleDefault {
		if m, ok := c.ModelByRole[role]; ok && strings.TrimSpace(m) != "" {
			return m
		}
	}
	return c.DefaultModel
}

// effectiveDeployment returns Deployment with the empty-string default
// resolved to DeploymentOpenAI.
func (c Config) effectiveDeployment() Deployment {
	if c.Deployment == "" {
		return DeploymentOpenAI
	}
	return c.Deployment
}
