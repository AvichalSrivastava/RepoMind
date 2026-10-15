package jira

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// AuthMode selects how the client authenticates to Jira.
type AuthMode string

const (
	// AuthBasic uses email + API token via HTTP Basic.
	AuthBasic AuthMode = "basic"
	// AuthBearer uses a Personal Access Token via Bearer auth.
	AuthBearer AuthMode = "bearer"
)

// Config controls the HTTP client. Build it from
// infrastructure/config.Config.Jira at the composition root.
type Config struct {
	BaseURL    string
	AuthMode   AuthMode
	Email      string        // required when AuthMode = AuthBasic
	APIToken   string        // required for both auth modes
	Timeout    time.Duration // per-request timeout; defaults to 30s
	MaxRetries int           // retry budget for 429 / 5xx; >= 0

	// QAOwnerFieldID is the Jira custom-field id for "QA Owner"
	// (e.g. "customfield_10173"). When set, ListSprintIssues
	// requests this field in its projection and decodes it as a
	// user display name onto Issue.QAOwner. Empty disables the
	// extraction and leaves Issue.QAOwner blank.
	QAOwnerFieldID string
}

// validate checks the Config for internal consistency. It does NOT
// mutate the receiver; defaults are applied inside NewClient.
func (c Config) validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("jira: base_url is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || !strings.HasPrefix(u.Scheme, "http") {
		return errors.New("jira: base_url must be a valid http(s) URL")
	}
	switch c.AuthMode {
	case AuthBasic:
		if c.Email == "" || c.APIToken == "" {
			return errors.New("jira: basic auth requires email and api_token")
		}
	case AuthBearer:
		if c.APIToken == "" {
			return errors.New("jira: bearer auth requires api_token")
		}
	default:
		return errors.New("jira: auth_mode must be basic or bearer")
	}
	if c.MaxRetries < 0 {
		return errors.New("jira: max_retries must be >= 0")
	}
	return nil
}
