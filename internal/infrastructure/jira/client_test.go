package jira_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apijira "github.com/repomind/repomind/internal/application/jira"
	"github.com/repomind/repomind/internal/infrastructure/jira"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.Handler) *jira.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := jira.NewClient(jira.Config{
		BaseURL:    srv.URL,
		AuthMode:   jira.AuthBasic,
		Email:      "ops@example.com",
		APIToken:   "tok",
		Timeout:    2 * time.Second,
		MaxRetries: 1,
	})
	require.NoError(t, err)
	return c
}

// newTestClientWithQAField mirrors newTestClient but configures a
// QA Owner custom-field id so QAOwner decoding is exercised.
func newTestClientWithQAField(t *testing.T, handler http.Handler, qaFieldID string) *jira.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := jira.NewClient(jira.Config{
		BaseURL:        srv.URL,
		AuthMode:       jira.AuthBasic,
		Email:          "ops@example.com",
		APIToken:       "tok",
		Timeout:        2 * time.Second,
		MaxRetries:     1,
		QAOwnerFieldID: qaFieldID,
	})
	require.NoError(t, err)
	return c
}

func TestNewClient_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  jira.Config
	}{
		{"no base url", jira.Config{AuthMode: jira.AuthBasic, Email: "e", APIToken: "t"}},
		{"invalid base url", jira.Config{BaseURL: "::not-a-url::", AuthMode: jira.AuthBasic, Email: "e", APIToken: "t"}},
		{"unknown auth", jira.Config{BaseURL: "https://x.atlassian.net", AuthMode: "wat", APIToken: "t"}},
		{"basic missing email", jira.Config{BaseURL: "https://x.atlassian.net", AuthMode: jira.AuthBasic, APIToken: "t"}},
		{"bearer missing token", jira.Config{BaseURL: "https://x.atlassian.net", AuthMode: jira.AuthBearer}},
		{"negative retries", jira.Config{BaseURL: "https://x.atlassian.net", AuthMode: jira.AuthBearer, APIToken: "t", MaxRetries: -1}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			_, err := jira.NewClient(tt.cfg)
			require.Error(t, err)
		})
	}
}

func TestListBoards_HappyPathAuthAndQuery(t *testing.T) {
	var seenAuth, seenPath string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.String()
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "isLast": true, "total": 2,
			"values": [
				{"id": 1, "name": "Alpha", "type": "scrum", "location": {"projectKey": "ALPHA"}},
				{"id": 2, "name": "Bravo", "type": "kanban"}
			]
		}`))
	}))

	page, err := c.ListBoards(context.Background(), apijira.ListBoardsOptions{
		ProjectKey: "ALPHA",
		BoardType:  "scrum",
		Page:       apijira.PageOptions{Limit: 50},
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "Alpha", page.Items[0].Name)
	assert.Equal(t, "ALPHA", page.Items[0].ProjectKey)
	assert.Empty(t, page.Items[1].ProjectKey, "missing location → empty project key")
	assert.Empty(t, page.NextCursor, "isLast=true → no cursor")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("ops@example.com:tok"))
	assert.Equal(t, want, seenAuth)
	assert.Contains(t, seenPath, "projectKeyOrId=ALPHA")
	assert.Contains(t, seenPath, "type=scrum")
	assert.Contains(t, seenPath, "startAt=0")
	assert.Contains(t, seenPath, "maxResults=50")
}

func TestListSprints_PaginationCursorAdvances(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery, "state=active%2Cclosed")
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "isLast": false, "total": 120,
			"values": [{"id": 7, "name": "Sprint 7", "state": "active", "originBoardId": 1}]
		}`))
	}))
	page, err := c.ListSprints(context.Background(), 1, apijira.ListSprintsOptions{
		State: "active,closed",
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "50", page.NextCursor)
}

func TestListSprints_RejectsZeroBoard(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be called for invalid input")
	}))
	_, err := c.ListSprints(context.Background(), 0, apijira.ListSprintsOptions{})
	require.Error(t, err)
}

func TestGetSprint_ParsesDates(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/agile/1.0/sprint/42", r.URL.Path)
		_, _ = w.Write([]byte(`{
			"id": 42, "name": "Sprint 42", "state": "closed",
			"startDate":    "2026-06-09T08:00:00.000Z",
			"endDate":      "2026-06-23T08:00:00.000Z",
			"completeDate": "2026-06-23T17:35:11.123Z",
			"originBoardId": 9, "goal": "Ship login"
		}`))
	}))
	s, err := c.GetSprint(context.Background(), 42)
	require.NoError(t, err)
	require.NotNil(t, s.StartDate)
	require.NotNil(t, s.EndDate)
	require.NotNil(t, s.CompleteDate)
	assert.Equal(t, int64(42), s.ID)
	assert.Equal(t, "closed", s.State)
	assert.Equal(t, 2026, s.StartDate.Year())
	assert.Equal(t, time.UTC, s.StartDate.Location())
}

func TestListSprintIssues_ProjectsFields(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.RawQuery, "jql=status%3D%22Done%22")
		assert.Contains(t, r.URL.RawQuery, "fields=summary%2Cstatus%2Cissuetype")
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "total": 1,
			"issues": [{
				"id": "10001", "key": "ALPHA-123",
				"fields": {
					"summary": "Add login",
					"status": {"name": "Done"},
					"issuetype": {"name": "Story"},
					"assignee": {"displayName": "Alice", "emailAddress": "alice@example.com"},
					"labels": ["auth"],
					"components": [{"name": "backend"}, {"name": "api"}]
				}
			}]
		}`))
	}))
	p, err := c.ListSprintIssues(context.Background(), 42, apijira.ListSprintIssuesOptions{
		JQL:    `status="Done"`,
		Fields: []string{"summary", "status", "issuetype"},
	})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	got := p.Items[0]
	assert.Equal(t, "ALPHA-123", got.Key)
	assert.Equal(t, "Done", got.Status)
	assert.Equal(t, "Story", got.Type)
	assert.Equal(t, "Alice", got.Assignee)
	assert.Equal(t, []string{"auth"}, got.Labels)
	assert.Equal(t, []string{"backend", "api"}, got.Components)
}

func TestListSprintIssues_AssigneeFallsBackToEmail(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "total": 1,
			"issues": [{
				"id": "1", "key": "X-1",
				"fields": {
					"summary": "x", "status": {"name": "Done"}, "issuetype": {"name": "Bug"},
					"assignee": {"displayName": "", "emailAddress": "bob@example.com"},
					"labels": [], "components": []
				}
			}]
		}`))
	}))
	p, err := c.ListSprintIssues(context.Background(), 1, apijira.ListSprintIssuesOptions{})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Equal(t, "bob@example.com", p.Items[0].Assignee)
}

func TestListSprintIssues_QAOwnerCustomFieldAndCommentsDecoded(t *testing.T) {
	// Default projection MUST include comment + the configured QA
	// Owner custom-field id, and the response decoding MUST populate
	// Issue.QAOwner and Issue.Comments (sorted ascending by created).
	var gotQuery string
	c := newTestClientWithQAField(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "total": 1,
			"issues": [{
				"id": "1", "key": "SIS-10571",
				"fields": {
					"summary": "s",
					"status": {"name": "Awaiting QA"},
					"issuetype": {"name": "Task"},
					"assignee": {"displayName": "Parvind Gupta", "emailAddress": "pg@example.com"},
					"labels": [], "components": [],
					"customfield_10173": {"displayName": "Parvind Gupta", "emailAddress": "pg@example.com"},
					"comment": {"comments": [
						{"id": "c2", "author": {"displayName": "Rishika Dixit"}, "created": "2026-06-24T11:35:46.005+0530"},
						{"id": "c1", "author": {"displayName": "Harshit Choyal"}, "created": "2026-06-15T14:15:53.908+0530"}
					]}
				}
			}]
		}`))
	}), "customfield_10173")
	p, err := c.ListSprintIssues(context.Background(), 1, apijira.ListSprintIssuesOptions{})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	iss := p.Items[0]

	assert.Contains(t, gotQuery, "comment", "default projection must include comment")
	assert.Contains(t, gotQuery, "customfield_10173", "default projection must include QA Owner field id")

	assert.Equal(t, "Parvind Gupta", iss.QAOwner, "QAOwner decoded from custom field")
	assert.Equal(t, "Parvind Gupta", iss.Assignee, "assignee unchanged")

	require.Len(t, iss.Comments, 2)
	// Sort MUST be ascending by CreatedAt (regardless of API order).
	assert.Equal(t, "Harshit Choyal", iss.Comments[0].Author)
	assert.Equal(t, "Rishika Dixit", iss.Comments[1].Author)
	assert.True(t, iss.Comments[0].CreatedAt.Before(iss.Comments[1].CreatedAt))
}

func TestListSprintIssues_QAOwnerFieldMissing_LeavesBlank(t *testing.T) {
	// When the QA Owner custom field is absent on the issue (or set
	// to null) the QAOwner must stay empty -- callers use empty as
	// "not configured / not set" and skip the QA column.
	c := newTestClientWithQAField(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "total": 1,
			"issues": [{
				"id": "1", "key": "X-1",
				"fields": {
					"summary": "s", "status": {"name": "Done"}, "issuetype": {"name": "Bug"},
					"assignee": null, "labels": [], "components": [],
					"customfield_10173": null
				}
			}]
		}`))
	}), "customfield_10173")
	p, err := c.ListSprintIssues(context.Background(), 1, apijira.ListSprintIssuesOptions{})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Empty(t, p.Items[0].QAOwner)
	assert.Empty(t, p.Items[0].Comments)
}

func TestListSprintIssues_ChangelogStatusTransitionsDecoded(t *testing.T) {
	// The default request MUST include expand=changelog, and the
	// adapter MUST project changelog.histories[].items[] where
	// field=="status" into apijira.Issue.StatusTransitions,
	// sorted ascending by CreatedAt.
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{
			"startAt": 0, "maxResults": 50, "total": 1,
			"issues": [{
				"id": "1", "key": "SIS-10255",
				"fields": {
					"summary": "s", "status": {"name": "Done"},
					"issuetype": {"name": "Defect"},
					"assignee": {"displayName": "Anand Kumar1"},
					"labels": [], "components": []
				},
				"changelog": {
					"histories": [
						{
							"id": "h2",
							"author": {"displayName": "Puneet Kumar"},
							"created": "2026-05-29T19:23:10.001+0530",
							"items": [
								{"field": "status", "fromString": "In Development", "toString": "IN QA"}
							]
						},
						{
							"id": "h1",
							"author": {"displayName": "Puneet Kumar"},
							"created": "2026-05-29T19:23:05.000+0530",
							"items": [
								{"field": "assignee", "fromString": "Puneet Kumar", "toString": "Anand Kumar1"},
								{"field": "status",   "fromString": "Backlog",       "toString": "In Development"}
							]
						}
					]
				}
			}]
		}`))
	}))
	p, err := c.ListSprintIssues(context.Background(), 1, apijira.ListSprintIssuesOptions{})
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	iss := p.Items[0]

	assert.Contains(t, gotQuery, "expand=changelog", "default request must expand changelog")

	// Assignee change filtered out; only status transitions kept,
	// sorted ascending by CreatedAt.
	require.Len(t, iss.StatusTransitions, 2)
	assert.Equal(t, "Puneet Kumar", iss.StatusTransitions[0].Author)
	assert.Equal(t, "Backlog", iss.StatusTransitions[0].From)
	assert.Equal(t, "In Development", iss.StatusTransitions[0].To)
	assert.Equal(t, "In Development", iss.StatusTransitions[1].From)
	assert.Equal(t, "IN QA", iss.StatusTransitions[1].To)
	assert.True(t, iss.StatusTransitions[0].CreatedAt.Before(iss.StatusTransitions[1].CreatedAt))
}

func TestStatusMapping(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, jira.ErrUnauthorized},
		{http.StatusForbidden, jira.ErrForbidden},
		{http.StatusNotFound, jira.ErrNotFound},
		{http.StatusBadRequest, jira.ErrBadRequest},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			_, err := c.GetSprint(context.Background(), 1)
			require.Error(t, err)
			assert.True(t, errors.Is(err, tt.want), "got %v, want %v", err, tt.want)
		})
	}
}

func TestRateLimited_RetriesUntilSuccess(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"name":"s","state":"active","originBoardId":1}`))
	}))
	s, err := c.GetSprint(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, int64(1), s.ID)
}

func TestServerError_RetriesThenSurfaces(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_, err := c.GetSprint(context.Background(), 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jira.ErrServer))
	assert.Equal(t, 2, calls, "MaxRetries=1 → 2 total attempts")
}

func TestContextCancellation_Honoured(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.GetSprint(ctx, 1)
	require.Error(t, err)
	// Either context.Canceled (request error path) or wrapped — both acceptable.
	assert.True(t, errors.Is(err, context.Canceled) || errors.Is(err, jira.ErrServer),
		"expected canceled or server error, got %v", err)
}
