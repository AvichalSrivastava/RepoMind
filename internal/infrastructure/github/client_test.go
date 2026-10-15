package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v63/github"

	apigithub "github.com/repomind/repomind/internal/application/github"
)

// helper: build a Client wired to an httptest server, bypassing App
// auth (which requires a real RSA key — covered separately in
// auth_test once we add live integration; out of unit scope).
func newTestClient(t *testing.T, srv *httptest.Server, repos []RepoSpec) *Client {
	t.Helper()
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	ghClient := gh.NewClient(nil)
	ghClient.BaseURL = u
	ghClient.UploadURL = u

	c, err := newClientWithGHClient(Config{
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPath: "unused-in-tests",
		MaxRetries:     2,
		Repos:          repos,
	}, ghClient)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestClient_RepoAllowList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be hit; got %s", r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	_, err := c.ListPullRequests(context.Background(), apigithub.RepositoryRef{Owner: "evil", Name: "repo"}, apigithub.ListPROptions{})
	if !errors.Is(err, ErrUnauthorizedRepo) {
		t.Fatalf("expected ErrUnauthorizedRepo, got %v", err)
	}
}

func TestClient_ListPullRequests_HappyPath(t *testing.T) {
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Errorf("state=%q want all", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "50" {
			t.Errorf("per_page=%q want 50", got)
		}
		// Advertise a next page via Link header (absolute URL).
		w.Header().Set("Link", `<`+srvURL+`/repos/SHL-India/TDM-A/pulls?page=2>; rel="next"`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"number":           42,
				"title":            "Fix login bug",
				"state":            "closed",
				"merged":           true,
				"merge_commit_sha": "abc123",
				"base":             map[string]any{"ref": "main"},
				"head":             map[string]any{"ref": "feature/login"},
				"user":             map[string]any{"login": "alice"},
				"html_url":         "https://github.com/SHL-India/TDM-A/pull/42",
				"created_at":       "2026-06-01T10:00:00Z",
				"updated_at":       "2026-06-02T10:00:00Z",
				"merged_at":        "2026-06-02T11:00:00Z",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	page, err := c.ListPullRequests(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"},
		apigithub.ListPROptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("want 1 PR, got %d", len(page.Items))
	}
	pr := page.Items[0]
	if pr.Number != 42 || pr.Title != "Fix login bug" || !pr.Merged || pr.MergeSHA != "abc123" {
		t.Errorf("PR mismatch: %+v", pr)
	}
	if pr.Author.Login != "alice" {
		t.Errorf("author login = %q", pr.Author.Login)
	}
	if pr.MergedAt == nil || pr.MergedAt.Year() != 2026 {
		t.Errorf("merged_at not parsed: %+v", pr.MergedAt)
	}
	if page.NextCursor != "2" {
		t.Errorf("next cursor = %q want 2", page.NextCursor)
	}
}

func TestClient_GetPullRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"number":7,"title":"Tiny","state":"open","base":{"ref":"main"},"head":{"ref":"x"},"user":{"login":"bob"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	pr, err := c.GetPullRequest(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 7)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pr.Number != 7 || pr.Title != "Tiny" || pr.Author.Login != "bob" {
		t.Errorf("pr mismatch: %+v", pr)
	}
}

func TestClient_GetPullRequest_RejectsBadNumber(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()),
		[]RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	_, err := c.GetPullRequest(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 0)
	if err == nil {
		t.Fatal("expected error for pr number 0")
	}
}

func TestClient_ListPullRequestFiles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"filename":"a.go","status":"modified","additions":3,"deletions":1,"changes":4}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	page, err := c.ListPullRequestFiles(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 9, apigithub.PageOptions{})
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Path != "a.go" || page.Items[0].Status != "modified" {
		t.Errorf("file page mismatch: %+v", page.Items)
	}
}

func TestClient_ListPullRequestCommits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/9/commits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"sha":"deadbeef","commit":{"message":"chore: tidy","committer":{"date":"2026-06-01T12:00:00Z","name":"Alice"}},"author":{"login":"alice"},"committer":{"login":"alice"}}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	page, err := c.ListPullRequestCommits(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 9, apigithub.PageOptions{})
	if err != nil {
		t.Fatalf("commits: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].SHA != "deadbeef" || page.Items[0].Timestamp.IsZero() {
		t.Errorf("commit page mismatch: %+v", page.Items)
	}
}

func TestClient_ListPullRequestReviews(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/9/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"id":111,"user":{"login":"carol"},"state":"APPROVED","body":"lgtm","submitted_at":"2026-06-01T13:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	page, err := c.ListPullRequestReviews(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 9, apigithub.PageOptions{})
	if err != nil {
		t.Fatalf("reviews: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != 111 || page.Items[0].State != "APPROVED" || page.Items[0].Reviewer.Login != "carol" {
		t.Errorf("review page mismatch: %+v", page.Items)
	}
}

func TestClient_CursorAdvance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("page"); got != "3" {
			t.Errorf("page=%q want 3", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	_, err := c.ListPullRequests(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"},
		apigithub.ListPROptions{Page: apigithub.PageOptions{Cursor: "3"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestClient_StatusMapping(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{401, ErrUnauthorized},
		{403, ErrForbidden},
		{404, ErrNotFound},
		{400, ErrBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/1", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = fmt.Fprintf(w, `{"message":"boom","documentation_url":"x"}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
			_, err := c.GetPullRequest(context.Background(),
				apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: want %v, got %v", tc.code, tc.want, err)
			}
		})
	}
}

func TestClient_RetriesOn500(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(500)
			_, _ = fmt.Fprintf(w, `{"message":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"number":1,"title":"ok","state":"open","base":{"ref":"main"},"head":{"ref":"f"},"user":{"login":"u"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pr, err := c.GetPullRequest(ctx, apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pr.Number != 1 {
		t.Errorf("pr.Number = %d", pr.Number)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("hits = %d want 3", hits)
	}
}

func TestClient_404DoesNotRetry(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/SHL-India/TDM-A/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(404)
		_, _ = fmt.Fprintf(w, `{"message":"nope"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv, []RepoSpec{{Owner: "SHL-India", Name: "TDM-A"}})
	_, err := c.GetPullRequest(context.Background(),
		apigithub.RepositoryRef{Owner: "SHL-India", Name: "TDM-A"}, 9)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d want 1 (no retries on 404)", got)
	}
}

func TestParseRepoList(t *testing.T) {
	specs, err := ParseRepoList(" SHL-India/TDM-A , SHL-India/MFS-B,shl-india/tdm-a ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("want 2 (dedup case-insensitive), got %d: %v", len(specs), specs)
	}
	if specs[0].String() != "SHL-India/TDM-A" || specs[1].String() != "SHL-India/MFS-B" {
		t.Errorf("specs = %v", specs)
	}

	if _, err := ParseRepoList(""); err == nil {
		t.Error("empty should fail")
	}
	if _, err := ParseRepoList("noslash"); err == nil {
		t.Error("noslash should fail")
	}
	if _, err := ParseRepoList("/onlyname"); err == nil {
		t.Error("missing owner should fail")
	}
	if _, err := ParseRepoList("owner/"); err == nil {
		t.Error("missing name should fail")
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{AppID: 1, InstallationID: 1, PrivateKeyPath: "x", Repos: []RepoSpec{{"o", "n"}}}
	if err := good.validate(); err != nil {
		t.Fatalf("good: %v", err)
	}

	bads := []struct {
		name string
		mut  func(*Config)
	}{
		{"no app id", func(c *Config) { c.AppID = 0 }},
		{"no install id", func(c *Config) { c.InstallationID = 0 }},
		{"no key path", func(c *Config) { c.PrivateKeyPath = "" }},
		{"bad url", func(c *Config) { c.BaseURL = "::not-a-url" }},
		{"neg retries", func(c *Config) { c.MaxRetries = -1 }},
		{"neg timeout", func(c *Config) { c.Timeout = -1 }},
		{"no repos", func(c *Config) { c.Repos = nil }},
	}
	for _, tc := range bads {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			if err := c.validate(); err == nil {
				t.Error("expected error")
			}
		})
	}
}

// Sanity: ensure our test helper actually points go-github at the
// httptest URL (otherwise tests would silently hit api.github.com).
func TestTestHelperPointsAtServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := newTestClient(t, srv, []RepoSpec{{Owner: "o", Name: "n"}})
	if !strings.HasPrefix(c.gh.BaseURL.String(), srv.URL) {
		t.Fatalf("client BaseURL %q does not start with %q", c.gh.BaseURL, srv.URL)
	}
}
