package jira

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	apijira "github.com/repomind/repomind/internal/application/jira"
)

// Wire-level JSON shapes. Unexported — these types never leak outside
// this package; every method that returns to the application layer
// converts to the port DTOs in internal/application/jira.

type boardListDTO struct {
	StartAt    int        `json:"startAt"`
	MaxResults int        `json:"maxResults"`
	IsLast     bool       `json:"isLast"`
	Total      int        `json:"total"`
	Values     []boardDTO `json:"values"`
}

type boardDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Location *struct {
		ProjectKey string `json:"projectKey"`
	} `json:"location,omitempty"`
}

func (b boardDTO) toPort() apijira.Board {
	out := apijira.Board{ID: b.ID, Name: b.Name, Type: b.Type}
	if b.Location != nil {
		out.ProjectKey = b.Location.ProjectKey
	}
	return out
}

type sprintListDTO struct {
	StartAt    int         `json:"startAt"`
	MaxResults int         `json:"maxResults"`
	IsLast     bool        `json:"isLast"`
	Total      int         `json:"total"`
	Values     []sprintDTO `json:"values"`
}

type sprintDTO struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	State         string    `json:"state"`
	StartDate     *jiraTime `json:"startDate,omitempty"`
	EndDate       *jiraTime `json:"endDate,omitempty"`
	CompleteDate  *jiraTime `json:"completeDate,omitempty"`
	OriginBoardID int64     `json:"originBoardId"`
	Goal          string    `json:"goal,omitempty"`
}

func (s sprintDTO) toPort() apijira.Sprint {
	return apijira.Sprint{
		ID:            s.ID,
		Name:          s.Name,
		State:         s.State,
		StartDate:     ptrTime(s.StartDate),
		EndDate:       ptrTime(s.EndDate),
		CompleteDate:  ptrTime(s.CompleteDate),
		OriginBoardID: s.OriginBoardID,
		Goal:          s.Goal,
	}
}

type issueListDTO struct {
	StartAt    int        `json:"startAt"`
	MaxResults int        `json:"maxResults"`
	Total      int        `json:"total"`
	Issues     []issueDTO `json:"issues"`
}

// jiraUserDTO is the shared wire shape for Jira user references
// (assignee, reporter, custom-field user pickers, comment authors).
// DisplayName is the canonical human name; EmailAddress is the
// fallback for cases where the person's profile display name is
// blank.
type jiraUserDTO struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// displayOrEmail returns the DisplayName, falling back to
// EmailAddress when the profile display name is blank. Returns ""
// when both fields are empty.
func (u *jiraUserDTO) displayOrEmail() string {
	if u == nil {
		return ""
	}
	if n := strings.TrimSpace(u.DisplayName); n != "" {
		return n
	}
	return strings.TrimSpace(u.EmailAddress)
}

type issueDTO struct {
	ID        string          `json:"id"`
	Key       string          `json:"key"`
	Fields    json.RawMessage `json:"fields"`
	Changelog *changelogDTO   `json:"changelog,omitempty"`
}

// changelogDTO matches the object returned when the search endpoint
// is called with `expand=changelog`. We only care about the
// histories list (which holds one entry per change event).
type changelogDTO struct {
	Histories []changelogHistoryDTO `json:"histories"`
}

// changelogHistoryDTO is one change event: an author performed one
// or more field changes at a point in time.
type changelogHistoryDTO struct {
	ID      string                    `json:"id"`
	Author  *jiraUserDTO              `json:"author"`
	Created string                    `json:"created"`
	Items   []changelogHistoryItemDTO `json:"items"`
}

// changelogHistoryItemDTO is one field change within a history
// event. Field == "status" is the one we care about; FromString /
// ToString carry the human-readable status names.
type changelogHistoryItemDTO struct {
	Field      string `json:"field"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

// issueFieldsDTO is the fixed-shape projection of the "fields"
// object we ALWAYS pull. Custom fields (QA Owner) are decoded via
// a separate map[string]json.RawMessage pass so the field id can
// come from Config at runtime.
type issueFieldsDTO struct {
	Summary string `json:"summary"`
	Status  struct {
		Name string `json:"name"`
	} `json:"status"`
	IssueType struct {
		Name string `json:"name"`
	} `json:"issuetype"`
	Assignee   *jiraUserDTO `json:"assignee,omitempty"`
	Reporter   *jiraUserDTO `json:"reporter,omitempty"`
	Labels     []string     `json:"labels"`
	Components []struct {
		Name string `json:"name"`
	} `json:"components"`
	Comment *commentContainerDTO `json:"comment,omitempty"`
}

// commentContainerDTO matches the wrapper Jira returns when the
// "comment" field is included in the projection:
//
//	{ "comments": [ {...}, {...} ], "total": N, "startAt": 0 }
//
// We only care about author + created; the ADF-formatted body is
// intentionally not decoded (never rendered by us; would bloat
// memory + JSON deserialisation work per ticket).
type commentContainerDTO struct {
	Comments []jiraCommentDTO `json:"comments"`
}

type jiraCommentDTO struct {
	ID      string       `json:"id"`
	Author  *jiraUserDTO `json:"author"`
	Created string       `json:"created"` // ISO-8601 with ms
}

// toPort converts an issueDTO to the application-layer DTO. The
// qaOwnerFieldID parameter selects which custom field carries the
// QA Owner value; empty disables extraction.
func (i issueDTO) toPort(qaOwnerFieldID string) apijira.Issue {
	out := apijira.Issue{
		ID:  i.ID,
		Key: i.Key,
	}
	if len(i.Fields) == 0 {
		return out
	}

	// Fixed-shape decode of the always-projected fields.
	var f issueFieldsDTO
	if err := json.Unmarshal(i.Fields, &f); err == nil {
		out.Summary = f.Summary
		out.Status = f.Status.Name
		out.Type = f.IssueType.Name
		out.Labels = append([]string(nil), f.Labels...)
		if f.Assignee != nil {
			out.Assignee = f.Assignee.displayOrEmail()
		}
		if f.Reporter != nil {
			out.Reporter = f.Reporter.displayOrEmail()
		}
		for _, c := range f.Components {
			out.Components = append(out.Components, c.Name)
		}
		if f.Comment != nil && len(f.Comment.Comments) > 0 {
			out.Comments = make([]apijira.IssueComment, 0, len(f.Comment.Comments))
			for _, c := range f.Comment.Comments {
				author := ""
				if c.Author != nil {
					author = c.Author.displayOrEmail()
				}
				out.Comments = append(out.Comments, apijira.IssueComment{
					Author:    author,
					CreatedAt: parseJiraTimestamp(c.Created),
				})
			}
			// Sort ascending by CreatedAt so the earliest-commenter
			// heuristic in the release-notes handler is deterministic
			// even if the API ever returns comments out of order.
			sort.SliceStable(out.Comments, func(a, b int) bool {
				return out.Comments[a].CreatedAt.Before(out.Comments[b].CreatedAt)
			})
		}
	}

	// Custom-field decode: pull QA Owner from the configured field
	// id. We keep this on a second decode pass so the field id can
	// change per Jira instance without a code change.
	if qaOwnerFieldID = strings.TrimSpace(qaOwnerFieldID); qaOwnerFieldID != "" {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(i.Fields, &raw); err == nil {
			if rawVal, ok := raw[qaOwnerFieldID]; ok && len(rawVal) > 0 && string(rawVal) != "null" {
				var u jiraUserDTO
				if err := json.Unmarshal(rawVal, &u); err == nil {
					out.QAOwner = u.displayOrEmail()
				}
			}
		}
	}

	// Changelog decode: pull status transitions out of the
	// changelog (only present when the caller requested
	// expand=changelog). We keep only "status" field changes and
	// sort ascending by CreatedAt so the release-notes handler
	// can pick the LAST transition into "In Development".
	if i.Changelog != nil && len(i.Changelog.Histories) > 0 {
		for _, h := range i.Changelog.Histories {
			author := ""
			if h.Author != nil {
				author = h.Author.displayOrEmail()
			}
			ts := parseJiraTimestamp(h.Created)
			for _, it := range h.Items {
				if !strings.EqualFold(it.Field, "status") {
					continue
				}
				out.StatusTransitions = append(out.StatusTransitions, apijira.IssueStatusTransition{
					Author:    author,
					From:      it.FromString,
					To:        it.ToString,
					CreatedAt: ts,
				})
			}
		}
		if len(out.StatusTransitions) > 1 {
			sort.SliceStable(out.StatusTransitions, func(a, b int) bool {
				return out.StatusTransitions[a].CreatedAt.Before(out.StatusTransitions[b].CreatedAt)
			})
		}
	}
	return out
}

// parseJiraTimestamp decodes Jira's ISO-8601 comment timestamps
// which look like "2026-06-15T14:15:53.908+0530". Returns the zero
// time on failure; callers use zero as "unknown" and sort accordingly.
func parseJiraTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	// Jira's format uses a numeric zone without a colon (e.g. +0530).
	// Try the layouts most likely to succeed in order.
	layouts := []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// jiraTime decodes Jira's ISO-8601 timestamps, which use millisecond
// precision (e.g. "2026-06-09T08:00:00.000Z"). RFC3339Nano covers that
// form; RFC3339 covers plain-second form. We try both for robustness.
type jiraTime struct {
	t time.Time
}

func (j *jiraTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
	}
	j.t = t.UTC()
	return nil
}

func ptrTime(jt *jiraTime) *time.Time {
	if jt == nil || jt.t.IsZero() {
		return nil
	}
	v := jt.t
	return &v
}
