// Package jira implements the application's apijira.Client port using
// Jira's REST API (Agile 1.0 + Platform). The client is safe for
// concurrent use and applies bounded retries with exponential backoff
// on 429 / 5xx responses.
//
// Authentication supports either Jira Cloud Basic (email + API token)
// or Bearer (Personal Access Token).
package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apijira "github.com/repomind/repomind/internal/application/jira"
)

const (
	agileBase           = "/rest/agile/1.0"
	defaultPageSize     = 50
	maxPageSize         = 100
	maxBackoff          = 5 * time.Second
	bodySnippetMaxBytes = 1024
)

// Compile-time guarantee that *Client satisfies the application port.
var _ apijira.Client = (*Client)(nil)

// Client is the concrete Jira API client.
type Client struct {
	cfg  Config
	http *http.Client
	auth string // pre-computed Authorization header value
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

	c := &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
	switch cfg.AuthMode {
	case AuthBasic:
		raw := cfg.Email + ":" + cfg.APIToken
		c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	case AuthBearer:
		c.auth = "Bearer " + cfg.APIToken
	}
	return c, nil
}

// ---------- Port implementation ----------

// ListBoards implements apijira.Client.
func (c *Client) ListBoards(ctx context.Context, opts apijira.ListBoardsOptions) (*apijira.BoardPage, error) {
	q := url.Values{}
	addPaging(q, opts.Page)
	if opts.ProjectKey != "" {
		q.Set("projectKeyOrId", opts.ProjectKey)
	}
	if opts.BoardType != "" {
		q.Set("type", opts.BoardType)
	}
	if opts.NameSubstring != "" {
		q.Set("name", opts.NameSubstring)
	}

	var raw boardListDTO
	if err := c.getJSON(ctx, agileBase+"/board?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	out := &apijira.BoardPage{
		NextCursor: nextCursor(raw.StartAt, raw.MaxResults, raw.IsLast, raw.Total),
	}
	for _, b := range raw.Values {
		out.Items = append(out.Items, b.toPort())
	}
	return out, nil
}

// ListSprints implements apijira.Client.
func (c *Client) ListSprints(ctx context.Context, boardID int64, opts apijira.ListSprintsOptions) (*apijira.SprintPage, error) {
	if boardID <= 0 {
		return nil, fmt.Errorf("jira: invalid board id %d", boardID)
	}
	q := url.Values{}
	addPaging(q, opts.Page)
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	path := fmt.Sprintf("%s/board/%d/sprint?%s", agileBase, boardID, q.Encode())

	var raw sprintListDTO
	if err := c.getJSON(ctx, path, &raw); err != nil {
		return nil, err
	}
	out := &apijira.SprintPage{
		NextCursor: nextCursor(raw.StartAt, raw.MaxResults, raw.IsLast, raw.Total),
	}
	for _, s := range raw.Values {
		out.Items = append(out.Items, s.toPort())
	}
	return out, nil
}

// GetSprint implements apijira.Client.
func (c *Client) GetSprint(ctx context.Context, sprintID int64) (*apijira.Sprint, error) {
	if sprintID <= 0 {
		return nil, fmt.Errorf("jira: invalid sprint id %d", sprintID)
	}
	var raw sprintDTO
	if err := c.getJSON(ctx, fmt.Sprintf("%s/sprint/%d", agileBase, sprintID), &raw); err != nil {
		return nil, err
	}
	s := raw.toPort()
	return &s, nil
}

// ListSprintIssues implements apijira.Client.
func (c *Client) ListSprintIssues(ctx context.Context, sprintID int64, opts apijira.ListSprintIssuesOptions) (*apijira.IssuePage, error) {
	if sprintID <= 0 {
		return nil, fmt.Errorf("jira: invalid sprint id %d", sprintID)
	}
	q := url.Values{}
	addPaging(q, opts.Page)
	if opts.JQL != "" {
		q.Set("jql", opts.JQL)
	}
	// Field projection: honour caller-supplied Fields when set,
	// otherwise fall back to the canonical projection needed by
	// the release-notes pipeline (summary/status/type/assignee/
	// labels/components + comment + configured QA Owner custom
	// field, when configured).
	fields := opts.Fields
	if len(fields) == 0 {
		fields = c.defaultIssueFields()
	}
	q.Set("fields", strings.Join(fields, ","))
	// Always request the changelog so the release-notes handler
	// can identify the dev owner as the author of the last
	// transition into an "In Development"-like status. The extra
	// payload is O(status-transitions-per-ticket) which is small
	// (typically < 10 per ticket).
	q.Set("expand", "changelog")
	path := fmt.Sprintf("%s/sprint/%d/issue?%s", agileBase, sprintID, q.Encode())

	var raw issueListDTO
	if err := c.getJSON(ctx, path, &raw); err != nil {
		return nil, err
	}
	// /sprint/{id}/issue does not return isLast; derive from totals.
	isLast := raw.StartAt+len(raw.Issues) >= raw.Total
	out := &apijira.IssuePage{
		NextCursor: nextCursor(raw.StartAt, raw.MaxResults, isLast, raw.Total),
	}
	for _, i := range raw.Issues {
		out.Items = append(out.Items, i.toPort(c.cfg.QAOwnerFieldID))
	}
	return out, nil
}

// defaultIssueFields returns the projection used when the caller
// does not supply an explicit field whitelist. Includes the QA
// Owner custom field when configured.
func (c *Client) defaultIssueFields() []string {
	base := []string{"summary", "status", "issuetype", "assignee", "reporter", "labels", "components", "comment"}
	if id := strings.TrimSpace(c.cfg.QAOwnerFieldID); id != "" {
		base = append(base, id)
	}
	return base
}

// ---------- HTTP helpers ----------

// getJSON performs a GET with retries on transient failures.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	full := c.cfg.BaseURL + path

	var lastErr error
	attempts := c.cfg.MaxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", c.auth)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "RepoMind/1.0")

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

// decodeStatus maps an HTTP response into either a successful unmarshal
// or one of the sentinel errors.
func decodeStatus(resp *http.Response, out any) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("jira: read body: %w", err)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("jira: decode body: %w", err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode >= 500:
		return ErrServer
	case resp.StatusCode == http.StatusBadRequest:
		return ErrBadRequest
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, bodySnippetMaxBytes))
		return fmt.Errorf("jira: unexpected status %d: %s", resp.StatusCode, string(snippet))
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

// ---------- Cursor encoding ----------
//
// The port exposes opaque string cursors. We encode the next startAt
// offset as a decimal string. Callers must not assume this format.

func addPaging(q url.Values, p apijira.PageOptions) {
	startAt := 0
	if p.Cursor != "" {
		if v, err := strconv.Atoi(p.Cursor); err == nil && v > 0 {
			startAt = v
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	q.Set("startAt", strconv.Itoa(startAt))
	q.Set("maxResults", strconv.Itoa(limit))
}

func nextCursor(startAt, maxResults int, isLast bool, total int) string {
	if isLast {
		return ""
	}
	next := startAt + maxResults
	if total > 0 && next >= total {
		return ""
	}
	return strconv.Itoa(next)
}
