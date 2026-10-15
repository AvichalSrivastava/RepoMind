package confluence

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	apiBase             = "/rest/api"
	bodySnippetMaxBytes = 1024
	maxBackoff          = 5 * time.Second
)

// Client is a thin HTTP client over the Confluence Cloud REST API.
// It deliberately does NOT know about markdown, section markers, or
// the publish port — those concerns live in publisher.go.
type Client struct {
	cfg  Config
	http *http.Client
	auth string // pre-computed Basic Authorization header
}

// NewClient validates cfg and constructs a Client.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	raw := cfg.Email + ":" + cfg.APIToken
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
		auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(raw)),
	}, nil
}

// Page is the slice of a Confluence content resource the publisher
// cares about.
type Page struct {
	ID          string
	Type        string
	Title       string
	Version     int
	BodyStorage string
	WebURL      string // fully-qualified human URL
}

// UpdateInput is the payload for PUT /rest/api/content/{id}. The
// caller is responsible for setting Version to current+1; the client
// does not auto-increment, because the publisher needs to be able to
// retry against a refreshed version after a 409.
type UpdateInput struct {
	ID          string
	Title       string
	Type        string // typically "page"
	NewVersion  int
	BodyStorage string
}

// CreateInput is the payload for POST /rest/api/content. Confluence
// creates the page as a child of ParentID; ParentID may be a page
// OR a folder (v1 REST accepts either — folders are internally
// page-like when referenced via ancestors[].id).
//
// SpaceKey is the human-readable key (e.g. "SHL360"); Confluence
// resolves it server-side, so callers don't need to look up the
// numeric space id.
type CreateInput struct {
	SpaceKey    string
	ParentID    string
	Title       string
	BodyStorage string
}

// GetPage fetches a page with body.storage and version expanded.
// Network / context errors propagate; HTTP failures are mapped to the
// package's sentinel errors.
func (c *Client) GetPage(ctx context.Context, id string) (*Page, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("confluence: page id is required")
	}
	// Build the query via net/url so the comma in "body.storage,version"
	// is percent-encoded to %2C, matching the canonical encoding the
	// Jira adapter (and Atlassian's own examples) uses.
	q := url.Values{}
	q.Set("expand", "body.storage,version")
	path := fmt.Sprintf("%s/content/%s?%s", apiBase, id, q.Encode())

	var raw pageDTO
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return toPage(raw, c.cfg.BaseURL), nil
}

// UpdatePage writes a new version of the page. Returns the
// representation Confluence echoes back (which contains the new
// version number and may include the rendered body / links).
func (c *Client) UpdatePage(ctx context.Context, in UpdateInput) (*Page, error) {
	if strings.TrimSpace(in.ID) == "" {
		return nil, errors.New("confluence: page id is required")
	}
	if in.NewVersion <= 0 {
		return nil, errors.New("confluence: new version must be positive")
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("confluence: title is required")
	}
	typ := in.Type
	if typ == "" {
		typ = "page"
	}

	body := updatePageRequestDTO{
		ID:    in.ID,
		Type:  typ,
		Title: in.Title,
		Version: updatePageVersionDTO{
			Number: in.NewVersion,
		},
		Body: updatePageBodyDTO{
			Storage: storageDTO{
				Value:          in.BodyStorage,
				Representation: "storage",
			},
		},
	}

	var raw pageDTO
	if err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("%s/content/%s", apiBase, in.ID), body, &raw); err != nil {
		return nil, err
	}
	return toPage(raw, c.cfg.BaseURL), nil
}

// CreatePage creates a new page as a child of ParentID inside the
// given space. The parent may be a normal page or a folder; both
// resolve through v1 REST's ancestors[] mechanism.
//
// The response DTO is the same pageDTO shape used by GetPage, so
// callers get back a fully-populated *Page (including WebURL) after
// a single round-trip.
func (c *Client) CreatePage(ctx context.Context, in CreateInput) (*Page, error) {
	if strings.TrimSpace(in.SpaceKey) == "" {
		return nil, errors.New("confluence: space key is required")
	}
	if strings.TrimSpace(in.ParentID) == "" {
		return nil, errors.New("confluence: parent id is required")
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("confluence: title is required")
	}

	body := createPageRequestDTO{
		Type:  "page",
		Title: in.Title,
		Space: createSpaceDTO{Key: in.SpaceKey},
		Ancestors: []createAncestorDTO{
			{ID: in.ParentID},
		},
		Body: updatePageBodyDTO{
			Storage: storageDTO{
				Value:          in.BodyStorage,
				Representation: "storage",
			},
		},
	}

	var raw pageDTO
	if err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("%s/content", apiBase), body, &raw); err != nil {
		return nil, err
	}
	return toPage(raw, c.cfg.BaseURL), nil
}

// failures (429, 5xx). 409 is treated as terminal here — the
// publisher handles the refetch-and-retry flow because it must
// re-apply its section splice against the latest body.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	full := c.cfg.BaseURL + path

	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("confluence: marshal body: %w", err)
		}
	}

	var lastErr error
	attempts := c.cfg.MaxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", c.auth)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "RepoMind/1.0")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if !networkRetryable(err) {
				return err
			}
			continue
		}

		decodeErr := decodeStatus(resp, out)
		_ = resp.Body.Close()
		if decodeErr == nil {
			return nil
		}
		lastErr = decodeErr
		if !errors.Is(decodeErr, ErrRateLimited) && !errors.Is(decodeErr, ErrServer) {
			return decodeErr
		}
	}
	return lastErr
}

func decodeStatus(resp *http.Response, out any) error {
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("confluence: read body: %w", err)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("confluence: decode body: %w", err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		return ErrConflict
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode >= 500:
		return ErrServer
	case resp.StatusCode == http.StatusBadRequest:
		return ErrBadRequest
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, bodySnippetMaxBytes))
		return fmt.Errorf("confluence: unexpected status %d: %s", resp.StatusCode, string(snippet))
	}
}

func networkRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func sleepBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(1<<uint(attempt)) * 200 * time.Millisecond
	if d > maxBackoff {
		d = maxBackoff
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// toPage projects the wire DTO into the public Page shape, building
// a fully-qualified web URL from the API's `_links.base` + `webui`
// when present (Atlassian returns the base independently for legacy
// reasons).
func toPage(p pageDTO, fallbackBase string) *Page {
	base := p.Links.Base
	if base == "" {
		base = strings.TrimRight(fallbackBase, "/")
	}
	url := ""
	if p.Links.WebUI != "" {
		url = strings.TrimRight(base, "/") + p.Links.WebUI
	}
	return &Page{
		ID:          p.ID,
		Type:        p.Type,
		Title:       p.Title,
		Version:     p.Version.Number,
		BodyStorage: p.Body.Storage.Value,
		WebURL:      url,
	}
}
