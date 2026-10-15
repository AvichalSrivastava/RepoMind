// Package github implements the application's apigithub.Client port.
//
// Auth: GitHub App installation (ghinstallation/v2) — see auth.go.
// Pagination: opaque base-10 page numbers (GitHub's REST `page`
// parameter). Callers must treat NextCursor as opaque.
// Retries: bounded exponential backoff on 429 / 5xx only.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v63/github"

	apigithub "github.com/repomind/repomind/internal/application/github"
)

const (
	defaultPerPage = 50
	maxPerPage     = 100
	maxBackoff     = 5 * time.Second
)

// Compile-time guarantee that *Client satisfies the application port.
var _ apigithub.Client = (*Client)(nil)

// Client is the concrete GitHub adapter.
type Client struct {
	cfg     Config
	gh      *gh.Client
	allowed map[string]struct{} // owner/name (lowercased) → allow-listed
}

// NewClient validates cfg and constructs a Client authenticated as the
// configured App installation.
func NewClient(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	httpClient, err := newInstallationHTTPClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("github: build installation transport: %w", err)
	}

	ghClient := gh.NewClient(httpClient)
	if cfg.BaseURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("github: parse base url: %w", err)
		}
		ghClient.BaseURL = u
		ghClient.UploadURL = u
	}

	allowed := make(map[string]struct{}, len(cfg.Repos))
	for _, r := range cfg.Repos {
		allowed[strings.ToLower(r.Owner+"/"+r.Name)] = struct{}{}
	}

	return &Client{cfg: cfg, gh: ghClient, allowed: allowed}, nil
}

// newClientWithGHClient is a test seam: lets unit tests inject a
// pre-built *gh.Client pointed at an httptest server, bypassing App
// auth (which needs a real RSA key).
func newClientWithGHClient(cfg Config, ghClient *gh.Client) (*Client, error) {
	allowed := make(map[string]struct{}, len(cfg.Repos))
	for _, r := range cfg.Repos {
		allowed[strings.ToLower(r.Owner+"/"+r.Name)] = struct{}{}
	}
	return &Client{cfg: cfg, gh: ghClient, allowed: allowed}, nil
}

// ---------- Port methods ----------

// ListPullRequests implements apigithub.Client.
func (c *Client) ListPullRequests(ctx context.Context, repo apigithub.RepositoryRef, opts apigithub.ListPROptions) (*apigithub.PullRequestPage, error) {
	if err := c.assertAllowed(repo); err != nil {
		return nil, err
	}

	listOpts := &gh.PullRequestListOptions{
		State:     normaliseState(opts.State),
		Base:      opts.Base,
		Sort:      "updated",
		Direction: orDefault(opts.SortDirection, "desc"),
		ListOptions: gh.ListOptions{
			Page:    decodeCursor(opts.Page.Cursor),
			PerPage: clampPerPage(opts.Page.Limit),
		},
	}

	var raw []*gh.PullRequest
	var resp *gh.Response
	err := c.do(ctx, func() error {
		var inner error
		raw, resp, inner = c.gh.PullRequests.List(ctx, repo.Owner, repo.Name, listOpts)
		return inner
	})
	if err != nil {
		return nil, err
	}

	out := &apigithub.PullRequestPage{NextCursor: encodeCursor(resp)}
	for _, p := range raw {
		out.Items = append(out.Items, toPortPR(p))
	}
	return out, nil
}

// GetPullRequest implements apigithub.Client.
func (c *Client) GetPullRequest(ctx context.Context, repo apigithub.RepositoryRef, number int) (*apigithub.PullRequest, error) {
	if err := c.assertAllowed(repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pr number %d", number)
	}

	var raw *gh.PullRequest
	err := c.do(ctx, func() error {
		var inner error
		raw, _, inner = c.gh.PullRequests.Get(ctx, repo.Owner, repo.Name, number)
		return inner
	})
	if err != nil {
		return nil, err
	}
	pr := toPortPR(raw)
	return &pr, nil
}

// ListPullRequestFiles implements apigithub.Client.
func (c *Client) ListPullRequestFiles(ctx context.Context, repo apigithub.RepositoryRef, number int, page apigithub.PageOptions) (*apigithub.FilePage, error) {
	if err := c.assertAllowed(repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pr number %d", number)
	}

	listOpts := &gh.ListOptions{Page: decodeCursor(page.Cursor), PerPage: clampPerPage(page.Limit)}

	var raw []*gh.CommitFile
	var resp *gh.Response
	err := c.do(ctx, func() error {
		var inner error
		raw, resp, inner = c.gh.PullRequests.ListFiles(ctx, repo.Owner, repo.Name, number, listOpts)
		return inner
	})
	if err != nil {
		return nil, err
	}

	out := &apigithub.FilePage{NextCursor: encodeCursor(resp)}
	for _, f := range raw {
		out.Items = append(out.Items, toPortFile(f))
	}
	return out, nil
}

// ListPullRequestCommits implements apigithub.Client.
func (c *Client) ListPullRequestCommits(ctx context.Context, repo apigithub.RepositoryRef, number int, page apigithub.PageOptions) (*apigithub.CommitPage, error) {
	if err := c.assertAllowed(repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pr number %d", number)
	}

	listOpts := &gh.ListOptions{Page: decodeCursor(page.Cursor), PerPage: clampPerPage(page.Limit)}

	var raw []*gh.RepositoryCommit
	var resp *gh.Response
	err := c.do(ctx, func() error {
		var inner error
		raw, resp, inner = c.gh.PullRequests.ListCommits(ctx, repo.Owner, repo.Name, number, listOpts)
		return inner
	})
	if err != nil {
		return nil, err
	}

	out := &apigithub.CommitPage{NextCursor: encodeCursor(resp)}
	for _, k := range raw {
		out.Items = append(out.Items, toPortCommit(k))
	}
	return out, nil
}

// ListPullRequestReviews implements apigithub.Client.
func (c *Client) ListPullRequestReviews(ctx context.Context, repo apigithub.RepositoryRef, number int, page apigithub.PageOptions) (*apigithub.ReviewPage, error) {
	if err := c.assertAllowed(repo); err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pr number %d", number)
	}

	listOpts := &gh.ListOptions{Page: decodeCursor(page.Cursor), PerPage: clampPerPage(page.Limit)}

	var raw []*gh.PullRequestReview
	var resp *gh.Response
	err := c.do(ctx, func() error {
		var inner error
		raw, resp, inner = c.gh.PullRequests.ListReviews(ctx, repo.Owner, repo.Name, number, listOpts)
		return inner
	})
	if err != nil {
		return nil, err
	}

	out := &apigithub.ReviewPage{NextCursor: encodeCursor(resp)}
	for _, r := range raw {
		out.Items = append(out.Items, toPortReview(r))
	}
	return out, nil
}

// ---------- Internal helpers ----------

// assertAllowed enforces the configured repo allow-list at the adapter
// boundary regardless of what the App could technically read.
func (c *Client) assertAllowed(r apigithub.RepositoryRef) error {
	if r.Owner == "" || r.Name == "" {
		return fmt.Errorf("github: empty repo ref")
	}
	if _, ok := c.allowed[strings.ToLower(r.Owner+"/"+r.Name)]; !ok {
		return fmt.Errorf("%w: %s/%s", ErrUnauthorizedRepo, r.Owner, r.Name)
	}
	return nil
}

// do executes f with bounded retries on 429/5xx. Other errors fail
// fast. The function is invoked anew each attempt — callers must keep
// it idempotent (all our calls are GETs, which they are).
func (c *Client) do(ctx context.Context, f func() error) error {
	attempts := c.cfg.MaxRetries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := sleepBackoff(ctx, i); err != nil {
				return err
			}
		}
		err := f()
		if err == nil {
			return nil
		}
		sentinel := classifyError(err)
		if sentinel == nil {
			return err // transport / context error: surface as-is
		}
		lastErr = fmt.Errorf("%w: %v", sentinel, err)
		if !errors.Is(sentinel, ErrRateLimited) && !errors.Is(sentinel, ErrServer) {
			return lastErr
		}
	}
	return lastErr
}

// classifyError maps go-github errors to our sentinels. Returns nil if
// err is not a recognised HTTP/API error (e.g. context cancellation or
// transport-level failures).
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	// Rate limit takes precedence — go-github surfaces it as its own
	// type whose HTTP status may be 403 or 429.
	var rle *gh.RateLimitError
	if errors.As(err, &rle) {
		return ErrRateLimited
	}
	var abe *gh.AbuseRateLimitError
	if errors.As(err, &abe) {
		return ErrRateLimited
	}
	var errResp *gh.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		switch errResp.Response.StatusCode {
		case 401:
			return ErrUnauthorized
		case 403:
			return ErrForbidden
		case 404:
			return ErrNotFound
		case 429:
			return ErrRateLimited
		case 400:
			return ErrBadRequest
		default:
			if errResp.Response.StatusCode >= 500 {
				return ErrServer
			}
			return ErrBadRequest
		}
	}
	return nil
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

// ---------- Pagination cursor ----------
//
// We expose opaque cursors at the port boundary but encode GitHub's
// page-number pagination underneath. Today they're decimal strings;
// tomorrow they could be GraphQL endCursors — callers never know.

func decodeCursor(c string) int {
	if c == "" {
		return 0
	}
	n, err := strconv.Atoi(c)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func encodeCursor(resp *gh.Response) string {
	if resp == nil || resp.NextPage == 0 {
		return ""
	}
	return strconv.Itoa(resp.NextPage)
}

func clampPerPage(limit int) int {
	if limit <= 0 {
		return defaultPerPage
	}
	if limit > maxPerPage {
		return maxPerPage
	}
	return limit
}

func normaliseState(s apigithub.PRState) string {
	switch s {
	case apigithub.PRStateOpen, apigithub.PRStateClosed, apigithub.PRStateAll:
		return string(s)
	default:
		return string(apigithub.PRStateAll)
	}
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
