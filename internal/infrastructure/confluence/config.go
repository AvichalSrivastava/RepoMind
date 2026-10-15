// Package confluence implements the application's publish.Publisher
// port against Confluence Cloud's REST API v1.
//
// The adapter is split into thin, testable units:
//
//   - config.go     — input shape + validation
//   - errors.go     — typed sentinels mapped from HTTP status codes
//   - dto.go        — wire-format DTOs
//   - markdown.go   — markdown → Confluence storage (XHTML) converter
//   - section.go    — locate / replace / append a named section
//     between repomind section markers
//   - client.go     — generic HTTP client (GetPage, UpdatePage)
//   - publisher.go  — bridges Client to the publish.Publisher port,
//     including section splice + 409 retry
//
// The split lets each concern have its own focused tests instead of
// one mega-table that combines marker handling, markdown rendering,
// and HTTP behavior.
package confluence

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Config controls the HTTP client. Build it from
// infrastructure/config.Config.Confluence at the composition root.
type Config struct {
	BaseURL    string        // e.g. https://acme.atlassian.net/wiki
	Email      string        // Atlassian account email (Basic auth user)
	APIToken   string        // Atlassian API token (Basic auth password)
	Timeout    time.Duration // per-request HTTP timeout; default 30s
	MaxRetries int           // 429 / 5xx / 409 retry budget; >= 0
}

// validate enforces internal consistency. It does NOT mutate the
// receiver; defaults are applied in NewClient.
func (c Config) validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("confluence: base_url is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("confluence: base_url must be a valid http(s) URL")
	}
	if strings.TrimSpace(c.Email) == "" {
		return errors.New("confluence: email is required")
	}
	if strings.TrimSpace(c.APIToken) == "" {
		return errors.New("confluence: api_token is required")
	}
	if c.MaxRetries < 0 {
		return errors.New("confluence: max_retries must be >= 0")
	}
	return nil
}
