package confluence_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/repomind/repomind/internal/infrastructure/confluence"
)

func newClient(t *testing.T, h http.Handler) *confluence.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := confluence.NewClient(confluence.Config{
		BaseURL:    srv.URL + "/wiki",
		Email:      "ops@example.com",
		APIToken:   "tok",
		Timeout:    2 * time.Second,
		MaxRetries: 2,
	})
	require.NoError(t, err)
	return c
}

func TestNewClient_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  confluence.Config
	}{
		{"missing base url", confluence.Config{Email: "e", APIToken: "t"}},
		{"invalid base url scheme", confluence.Config{BaseURL: "ftp://x", Email: "e", APIToken: "t"}},
		{"malformed base url", confluence.Config{BaseURL: "::not-a-url::", Email: "e", APIToken: "t"}},
		{"missing email", confluence.Config{BaseURL: "https://x.atlassian.net/wiki", APIToken: "t"}},
		{"missing api token", confluence.Config{BaseURL: "https://x.atlassian.net/wiki", Email: "e"}},
		{"negative retries", confluence.Config{BaseURL: "https://x.atlassian.net/wiki", Email: "e", APIToken: "t", MaxRetries: -1}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := confluence.NewClient(tt.cfg)
			require.Error(t, err)
		})
	}
}

func TestGetPage_HappyPathAuthAndQuery(t *testing.T) {
	var seenAuth, seenPath string
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.String()
		_, _ = w.Write([]byte(`{
			"id": "12345", "type": "page", "title": "Release Notes",
			"version": {"number": 7},
			"body": {"storage": {"value": "<p>hello</p>", "representation": "storage"}},
			"_links": {"base": "https://acme.atlassian.net/wiki", "webui": "/spaces/X/pages/12345/Release+Notes"}
		}`))
	}))

	got, err := c.GetPage(context.Background(), "12345")
	require.NoError(t, err)
	assert.Equal(t, "12345", got.ID)
	assert.Equal(t, "Release Notes", got.Title)
	assert.Equal(t, 7, got.Version)
	assert.Equal(t, "<p>hello</p>", got.BodyStorage)
	assert.Equal(t, "https://acme.atlassian.net/wiki/spaces/X/pages/12345/Release+Notes", got.WebURL)

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops@example.com:tok"))
	assert.Equal(t, want, seenAuth)
	assert.Contains(t, seenPath, "/wiki/rest/api/content/12345")
	assert.Contains(t, seenPath, "expand=body.storage%2Cversion")
}

func TestGetPage_RejectsEmptyID(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for empty id")
	}))
	_, err := c.GetPage(context.Background(), " ")
	require.Error(t, err)
}

func TestGetPage_MapsStatusCodesToSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"401 -> Unauthorized", http.StatusUnauthorized, confluence.ErrUnauthorized},
		{"403 -> Forbidden", http.StatusForbidden, confluence.ErrForbidden},
		{"404 -> NotFound", http.StatusNotFound, confluence.ErrNotFound},
		{"409 -> Conflict", http.StatusConflict, confluence.ErrConflict},
		{"400 -> BadRequest", http.StatusBadRequest, confluence.ErrBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			_, err := c.GetPage(context.Background(), "1")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestGetPage_RetriesOn429ThenSucceeds(t *testing.T) {
	var calls int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","type":"page","title":"t","version":{"number":1},"body":{"storage":{"value":"","representation":"storage"}}}`))
	}))
	_, err := c.GetPage(context.Background(), "1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, atomic.LoadInt32(&calls))
}

func TestGetPage_ExhaustsRetriesAndReturnsLastErr(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.GetPage(context.Background(), "1")
	require.ErrorIs(t, err, confluence.ErrServer)
}

func TestUpdatePage_SendsCorrectShape(t *testing.T) {
	var seenMethod, seenPath, seenContentType string
	var bodyBytes []byte
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		seenContentType = r.Header.Get("Content-Type")
		bodyBytes, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"42","type":"page","title":"T","version":{"number":9},"body":{"storage":{"value":"<p>n</p>","representation":"storage"}},"_links":{"base":"https://acme.atlassian.net/wiki","webui":"/x/42"}}`))
	}))

	updated, err := c.UpdatePage(context.Background(), confluence.UpdateInput{
		ID:          "42",
		Title:       "T",
		NewVersion:  9,
		BodyStorage: "<p>n</p>",
	})
	require.NoError(t, err)
	assert.Equal(t, 9, updated.Version)
	assert.Equal(t, "https://acme.atlassian.net/wiki/x/42", updated.WebURL)

	assert.Equal(t, http.MethodPut, seenMethod)
	assert.Equal(t, "/wiki/rest/api/content/42", seenPath)
	assert.Equal(t, "application/json", seenContentType)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(bodyBytes, &sent))
	assert.Equal(t, "42", sent["id"])
	assert.Equal(t, "page", sent["type"], "Type defaults to \"page\" when caller omits it")
	assert.Equal(t, "T", sent["title"])
	require.Contains(t, sent, "version")
	require.Contains(t, sent, "body")

	version := sent["version"].(map[string]any)
	assert.EqualValues(t, 9, version["number"])

	storage := sent["body"].(map[string]any)["storage"].(map[string]any)
	assert.Equal(t, "<p>n</p>", storage["value"])
	assert.Equal(t, "storage", storage["representation"])
}

func TestUpdatePage_ConflictSurfacedNotRetried(t *testing.T) {
	// Inner client must NOT auto-retry 409 — the publisher does it,
	// because the body needs re-splicing against the latest GET.
	var calls int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
	}))
	_, err := c.UpdatePage(context.Background(), confluence.UpdateInput{
		ID: "1", Title: "T", NewVersion: 2, BodyStorage: "x",
	})
	require.ErrorIs(t, err, confluence.ErrConflict)
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls), "client must not retry 409 internally")
}

func TestUpdatePage_RejectsZeroVersionAndEmptyTitle(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for invalid input")
	}))
	_, err := c.UpdatePage(context.Background(), confluence.UpdateInput{ID: "1", Title: "T", NewVersion: 0})
	require.Error(t, err)
	_, err = c.UpdatePage(context.Background(), confluence.UpdateInput{ID: "1", Title: "", NewVersion: 1})
	require.Error(t, err)
}

func TestGetPage_PropagatesContextCancellation(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.GetPage(ctx, "1")
	require.Error(t, err)
	// Either the context error or the underlying *url.Error wrapping it.
	assert.True(t,
		strings.Contains(err.Error(), "context canceled") ||
			strings.Contains(err.Error(), "canceled"),
		"unexpected error: %v", err)
}
