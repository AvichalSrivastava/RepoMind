package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/analysis/releasenotes"
	"github.com/repomind/repomind/internal/application/analysis/ticketsummary"
	apijira "github.com/repomind/repomind/internal/application/jira"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/application/publish"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/release"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// ---- fakes ---------------------------------------------------------

type fakeSprints struct {
	mu     sync.Mutex
	byID   map[shared.SprintID]*sprint.Sprint
	getErr error
}

func newFakeSprints() *fakeSprints { return &fakeSprints{byID: map[shared.SprintID]*sprint.Sprint{}} }

func (f *fakeSprints) Save(_ context.Context, s *sprint.Sprint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[s.ID()] = s
	return nil
}
func (f *fakeSprints) FindByID(_ context.Context, id shared.SprintID) (*sprint.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.byID[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return s, nil
}
func (f *fakeSprints) FindByJiraID(_ context.Context, jiraID int64) (*sprint.Sprint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.byID {
		if s.JiraSprintID() == jiraID {
			return s, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeSprints) FindActiveByTeam(context.Context, string) (*sprint.Sprint, error) {
	return nil, persistence.ErrNotFound
}

type fakeReleases struct {
	mu    sync.Mutex
	bySpr map[shared.SprintID][]*release.Release // chronological
	saved []*release.Release
}

func newFakeReleases() *fakeReleases {
	return &fakeReleases{bySpr: map[shared.SprintID][]*release.Release{}}
}
func (r *fakeReleases) Save(_ context.Context, rel *release.Release) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = append(r.saved, rel)
	// Update existing in-place if matching ID, else append.
	list := r.bySpr[rel.SprintID()]
	for i, existing := range list {
		if existing.ID() == rel.ID() {
			list[i] = rel
			r.bySpr[rel.SprintID()] = list
			return nil
		}
	}
	r.bySpr[rel.SprintID()] = append(list, rel)
	return nil
}
func (r *fakeReleases) FindByID(context.Context, shared.ReleaseID) (*release.Release, error) {
	return nil, persistence.ErrNotFound
}
func (r *fakeReleases) FindBySprintVersion(context.Context, shared.SprintID, release.Version) (*release.Release, error) {
	return nil, persistence.ErrNotFound
}
func (r *fakeReleases) ListBySprint(_ context.Context, id shared.SprintID) ([]*release.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*release.Release, len(r.bySpr[id]))
	copy(out, r.bySpr[id])
	return out, nil
}
func (r *fakeReleases) FindLatestBySprint(_ context.Context, id shared.SprintID) (*release.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.bySpr[id]
	if len(list) == 0 {
		return nil, persistence.ErrNotFound
	}
	return list[len(list)-1], nil
}

type fakeJira struct {
	keys   []string        // shorthand: keys with default Summary="stub"
	issues []apijira.Issue // full overrides (used verbatim when non-empty)
	err    error
}

func (f *fakeJira) ListBoards(context.Context, apijira.ListBoardsOptions) (*apijira.BoardPage, error) {
	return nil, nil
}
func (f *fakeJira) ListSprints(context.Context, int64, apijira.ListSprintsOptions) (*apijira.SprintPage, error) {
	return nil, nil
}
func (f *fakeJira) GetSprint(context.Context, int64) (*apijira.Sprint, error) {
	return nil, nil
}
func (f *fakeJira) ListSprintIssues(_ context.Context, _ int64, _ apijira.ListSprintIssuesOptions) (*apijira.IssuePage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.issues) > 0 {
		return &apijira.IssuePage{Items: append([]apijira.Issue(nil), f.issues...)}, nil
	}
	items := make([]apijira.Issue, 0, len(f.keys))
	for _, k := range f.keys {
		items = append(items, apijira.Issue{Key: k, Summary: "stub"})
	}
	return &apijira.IssuePage{Items: items}, nil
}

type fakeComposer struct {
	called int
	err    error
	notes  analysis.ReleaseNotes
}

func (c *fakeComposer) Analyze(_ context.Context, in releasenotes.Input) (*analysis.AnalysisResult, error) {
	c.called++
	if c.err != nil {
		return nil, c.err
	}
	return analysis.NewResult(
		analysis.KindReleaseNotes,
		analysis.SubjectForSprint(in.Sprint.ID()),
		analysis.AnalyzerRef{Name: "release_notes", Version: "1.0.0", PromptVersion: "1.0.0"},
		c.notes,
		"gpt-test",
		123,
		shared.SystemClock{},
	)
}

type fakePublisher struct {
	called  int
	calls   []publish.Input
	err     error
	pageURL string
}

func (p *fakePublisher) Publish(_ context.Context, in publish.Input) (*publish.Result, error) {
	p.called++
	p.calls = append(p.calls, in)
	if p.err != nil {
		return nil, p.err
	}
	url := p.pageURL
	if url == "" {
		url = "https://example.atlassian.net/wiki/spaces/X/pages/" + in.PageID
	}
	return &publish.Result{PageID: in.PageID, NewVersion: 2, URL: url}, nil
}

// ---- helpers ---------------------------------------------------------

// mkSprint returns a Sprint aggregate ready to use in tests.
func mkSprint(t *testing.T, jiraID int64, state sprint.State) *sprint.Sprint {
	t.Helper()
	startsAt := time.Now().Add(-14 * 24 * time.Hour)
	endsAt := time.Now()
	in := sprint.MirrorInput{
		JiraSprintID: jiraID,
		BoardID:      1,
		Team:         "Marvelous",
		Name:         "Marvelous_Sprint_X",
		State:        state,
		Goal:         "deliver value",
		StartsAt:     &startsAt,
		EndsAt:       &endsAt,
	}
	if state == sprint.StateClosed {
		in.CompletedAt = &endsAt
	}
	s, err := sprint.MirrorFromJira(in, shared.SystemClock{})
	if err != nil {
		t.Fatalf("mkSprint: %v", err)
	}
	return s
}

// mkMergedPR returns a PullRequest pre-merged with the given SHA and
// associated with the given ticket keys.
func mkMergedPR(t *testing.T, repoID shared.RepositoryID, num int, mergeSHA string, tickets ...string) *pullrequest.PullRequest {
	t.Helper()
	author, _ := shared.NewAuthor("octocat", "Octo Cat", "octo@example.com")
	src, _ := shared.NewBranch("feature/x")
	dst, _ := shared.NewBranch("main")
	pr, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: repoID,
		Number:       pullrequest.Number(num),
		Title:        "feat: do the thing",
		Description:  "implements " + tickets[0],
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("Open PR: %v", err)
	}
	for _, k := range tickets {
		tk, _ := shared.NewTicketKey(k)
		pr.AssociateTicket(tk)
	}
	sha, _ := shared.NewSHA(mergeSHA)
	if err := pr.Merge(time.Now(), sha); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return pr
}

// stubNotes returns a valid analysis.ReleaseNotes for the composer fake.
func stubNotes(prID shared.PullRequestID) analysis.ReleaseNotes {
	tk, _ := shared.NewTicketKey("SIS-1")
	rn, _ := analysis.NewReleaseNotes(
		"One paragraph summary of what landed.",
		"## Features\n- feat: do the thing (SIS-1)\n",
		[]analysis.ReleaseNoteEntry{
			{
				Category: analysis.CategoryFeature,
				Title:    "feat: do the thing",
				PR:       prID,
				Tickets:  []shared.TicketKey{tk},
			},
		},
	)
	return rn
}

// buildRNHandler wires the fakes into a working handler.
func buildRNHandler(t *testing.T, j *fakeJira, sprints *fakeSprints, prs *fakePRs, releases *fakeReleases, an *fakeAnalyses, comp *fakeComposer, pub *fakePublisher, skip time.Duration) *ReleaseNotesHandler {
	t.Helper()
	stores := persistence.Stores{
		Sprints:      sprints,
		PullRequests: prs,
		Releases:     releases,
		Analyses:     an,
	}
	h, err := NewReleaseNotesHandler(ReleaseNotesHandlerDeps{
		Jira:             j,
		Stores:           stores,
		UoW:              &trivialUoW{s: stores},
		Composer:         comp,
		Publisher:        pub,
		ConfluencePageID: "123456",
		HeadingPrefix:    "Marvelous",
		Logger:           discardLogger(),
		Clock:            shared.SystemClock{},
		SkipWindow:       skip,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return h
}

func mkRNEvent(t *testing.T, sprintID uuid.UUID) persistence.OutboxEvent {
	t.Helper()
	body, _ := json.Marshal(ReleaseNotesRequestPayload{
		SprintID: sprintID,
		Reason:   "operator",
	})
	return persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventReleaseNotesRequested),
		Payload:   body,
	}
}

// ---- tests ---------------------------------------------------------

func TestNewReleaseNotesHandler_RejectsNilDeps(t *testing.T) {
	full := ReleaseNotesHandlerDeps{
		Jira:             &fakeJira{},
		Stores:           persistence.Stores{Sprints: newFakeSprints(), PullRequests: newFakePRs(), Releases: newFakeReleases(), Analyses: &fakeAnalyses{}},
		Composer:         &fakeComposer{},
		Publisher:        &fakePublisher{},
		ConfluencePageID: "x",
		Logger:           discardLogger(),
	}
	full.UoW = &trivialUoW{s: full.Stores}

	type mut func(d *ReleaseNotesHandlerDeps)
	cases := []struct {
		name string
		m    mut
	}{
		{"nil jira", func(d *ReleaseNotesHandlerDeps) { d.Jira = nil }},
		{"nil sprints", func(d *ReleaseNotesHandlerDeps) { d.Stores.Sprints = nil }},
		{"nil prs", func(d *ReleaseNotesHandlerDeps) { d.Stores.PullRequests = nil }},
		{"nil releases", func(d *ReleaseNotesHandlerDeps) { d.Stores.Releases = nil }},
		{"nil analyses", func(d *ReleaseNotesHandlerDeps) { d.Stores.Analyses = nil }},
		{"nil uow", func(d *ReleaseNotesHandlerDeps) { d.UoW = nil }},
		{"nil publisher", func(d *ReleaseNotesHandlerDeps) { d.Publisher = nil }},
		{"empty page id", func(d *ReleaseNotesHandlerDeps) { d.ConfluencePageID = "" }},
		{"nil logger", func(d *ReleaseNotesHandlerDeps) { d.Logger = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := full
			c.m(&d)
			if _, err := NewReleaseNotesHandler(d); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestReleaseNotesHandler_SprintNotFound_ReturnsError(t *testing.T) {
	sprints := newFakeSprints()
	h := buildRNHandler(t, &fakeJira{}, sprints, newFakePRs(), newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, &fakePublisher{}, 0)
	evt := mkRNEvent(t, uuid.New()) // sprint not in DB
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestReleaseNotesHandler_NoTickets_NoOp(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateActive)
	_ = sprints.Save(context.Background(), sp)

	pub := &fakePublisher{}
	comp := &fakeComposer{}
	h := buildRNHandler(t, &fakeJira{keys: nil}, sprints, newFakePRs(), newFakeReleases(),
		&fakeAnalyses{}, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if comp.called != 0 {
		t.Errorf("composer should not have run; called=%d", comp.called)
	}
	if pub.called != 0 {
		t.Errorf("publisher should not have run; called=%d", pub.called)
	}
}

func TestReleaseNotesHandler_NoMergedPRs_PublishesTicketOnlyPage(t *testing.T) {
	// M11: when Jira has tickets but no merged PRs are matched yet, the
	// handler still publishes a table with one row per Jira ticket. Dev
	// owner falls back to the Jira assignee; description falls back to
	// the Jira summary. No release row is persisted (nothing delivered).
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	// PR exists but is OPEN, not merged -> filtered out.
	prs := newFakePRs()
	author, _ := shared.NewAuthor("octo", "Octo", "o@e.com")
	src, _ := shared.NewBranch("f/x")
	dst, _ := shared.NewBranch("main")
	openPR, _ := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(), Number: 1,
		Title: "wip", Author: author, SourceBranch: src, TargetBranch: dst,
	}, shared.SystemClock{})
	tk, _ := shared.NewTicketKey("SIS-1")
	openPR.AssociateTicket(tk)
	_ = prs.Save(context.Background(), openPR)

	pub := &fakePublisher{}
	comp := &fakeComposer{}
	releases := newFakeReleases()
	j := &fakeJira{issues: []apijira.Issue{{
		Key: "SIS-1", Summary: "Ticket-only summary", Assignee: "alice",
	}}}
	h := buildRNHandler(t, j, sprints, prs, releases,
		&fakeAnalyses{}, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if comp.called != 0 {
		t.Errorf("composer should not have run (no merged PRs); called=%d", comp.called)
	}
	if pub.called != 1 {
		t.Fatalf("publisher should have run once with tickets-only page; called=%d", pub.called)
	}
	if len(releases.saved) != 0 {
		t.Errorf("no release row should be persisted for zero-PR publish; saved=%d", len(releases.saved))
	}
	rows := pub.calls[0].LogRows
	if len(rows) != 1 {
		t.Fatalf("want 1 row for 1 ticket; got %d", len(rows))
	}
	if rows[0].TicketKey != "SIS-1" {
		t.Errorf("ticket key: got %q want SIS-1", rows[0].TicketKey)
	}
	if rows[0].Description != "Ticket-only summary" {
		t.Errorf("description should fall back to Jira summary; got %q", rows[0].Description)
	}
	if len(rows[0].DevOwners) != 1 || rows[0].DevOwners[0] != "alice" {
		t.Errorf("dev owner should fall back to Jira assignee; got %v", rows[0].DevOwners)
	}
	if len(rows[0].PRs) != 0 {
		t.Errorf("no PRs should be attached (none merged); got %d", len(rows[0].PRs))
	}
}

func TestReleaseNotesHandler_NoMergedPRs_QAOwnerPopulated_DevBacktrackedFromComments(t *testing.T) {
	// SIS-10571 shape: current Jira assignee has been flipped to the
	// QA person after dev-complete. The QA Owner custom field carries
	// the same person. The handler must:
	//   1. put the QA Owner name into QAOwners (not DevOwners),
	//   2. NOT put the current assignee into DevOwners (equals QA),
	//   3. backtrack the dev owner to the earliest non-QA commenter.
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	j := &fakeJira{issues: []apijira.Issue{{
		Key:      "SIS-1",
		Summary:  "Ticket-only summary",
		Assignee: "Parvind Gupta", // flipped to QA after dev-complete
		QAOwner:  "Parvind Gupta", // canonical QA Owner
		Comments: []apijira.IssueComment{
			{Author: "Harshit Choyal", CreatedAt: time.Date(2026, 6, 15, 14, 15, 0, 0, time.UTC)},
			{Author: "Rishika Dixit", CreatedAt: time.Date(2026, 6, 24, 11, 35, 0, 0, time.UTC)},
		},
	}}}
	pub := &fakePublisher{}
	comp := &fakeComposer{}
	releases := newFakeReleases()

	h := buildRNHandler(t, j, sprints, prs, releases,
		&fakeAnalyses{}, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if pub.called != 1 {
		t.Fatalf("publisher should have run once; called=%d", pub.called)
	}
	rows := pub.calls[0].LogRows
	if len(rows) != 1 {
		t.Fatalf("want 1 row; got %d", len(rows))
	}

	// Dev owner MUST be the earliest non-QA commenter.
	if len(rows[0].DevOwners) != 1 || rows[0].DevOwners[0] != "Harshit Choyal" {
		t.Errorf("dev owner should be earliest non-QA commenter 'Harshit Choyal'; got %v", rows[0].DevOwners)
	}
	// QA owner MUST come from the custom field.
	if len(rows[0].QAOwners) != 1 || rows[0].QAOwners[0] != "Parvind Gupta" {
		t.Errorf("QA owner should be 'Parvind Gupta' from QAOwner field; got %v", rows[0].QAOwners)
	}
}

func TestReleaseNotesHandler_QAOwner_FallsBackToReporter_WhenCustomFieldEmpty(t *testing.T) {
	// When the QA Owner custom field is unset on the ticket the
	// Reporter is used as the QA owner. This matches product intent:
	// the person who filed the ticket is a reasonable proxy for who
	// signs off on it. Reporter is NEVER the (post-flip) current
	// Assignee, so this fallback is safe.
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	j := &fakeJira{issues: []apijira.Issue{{
		Key:      "SIS-1",
		Summary:  "s",
		Assignee: "Anand Kumar1", // post-flip assignee, must NOT be QA
		Reporter: "Divya Gupta",  // reporter should surface as QA
		QAOwner:  "",             // custom field unset
	}}}
	pub := &fakePublisher{}

	h := buildRNHandler(t, j, sprints, prs, newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	row := pub.calls[0].LogRows[0]
	if len(row.QAOwners) != 1 || row.QAOwners[0] != "Divya Gupta" {
		t.Errorf("QA owner should fall back to reporter when custom field empty; got %v", row.QAOwners)
	}
}

func TestReleaseNotesHandler_DevOwnerFromStatusTransition_BeatsComments(t *testing.T) {
	// SIS-10255 shape: reporter=Anand Kumar1 opens a defect and
	// comments first on his own ticket; Puneet Kumar then picks it
	// up and moves the status Backlog -> In Development (and
	// simultaneously flips the assignee to Anand Kumar1 in this
	// workflow). The changelog is the authoritative source for the
	// dev owner; it MUST beat the comment-based fallback.
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	t0 := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	j := &fakeJira{issues: []apijira.Issue{{
		Key:      "SIS-10255",
		Summary:  "UI misalignment in Select People",
		Assignee: "Anand Kumar1", // flipped by the "Puneet moves it into dev" change event
		Comments: []apijira.IssueComment{
			// Reporter posts an early clarification -- earliest
			// non-QA non-assignee commenter is empty (Anand is
			// the assignee), so the comment-picker would return "".
			// The changelog transition MUST fill the gap.
			{Author: "Anand Kumar1", CreatedAt: t0},
			{Author: "Rishika Dixit", CreatedAt: t0.Add(48 * time.Hour)},
		},
		StatusTransitions: []apijira.IssueStatusTransition{
			{Author: "Puneet Kumar", From: "Backlog", To: "In Development", CreatedAt: t0.Add(72 * time.Hour)},
			{Author: "Puneet Kumar", From: "In Development", To: "IN QA", CreatedAt: t0.Add(72*time.Hour + time.Second)},
		},
	}}}
	pub := &fakePublisher{}

	h := buildRNHandler(t, j, sprints, prs, newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	row := pub.calls[0].LogRows[0]
	if len(row.DevOwners) != 1 || row.DevOwners[0] != "Puneet Kumar" {
		t.Errorf("dev owner should come from the Backlog->In Development transition; got %v", row.DevOwners)
	}
}

func TestReleaseNotesHandler_DevOwnerFromStatusTransition_PicksLatestReentry(t *testing.T) {
	// If a ticket bounces out of dev and back in again, the LATEST
	// mover into an "in development"-like status is the current
	// dev owner. Verifies the reverse-order scan in
	// pickDevFromTransitions.
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	j := &fakeJira{issues: []apijira.Issue{{
		Key: "SIS-1", Summary: "s",
		Assignee: "Some QA",
		StatusTransitions: []apijira.IssueStatusTransition{
			{Author: "Dev One", From: "Backlog", To: "In Development", CreatedAt: t0},
			{Author: "Some QA", From: "In Development", To: "IN QA", CreatedAt: t0.Add(24 * time.Hour)},
			{Author: "Dev Two", From: "IN QA", To: "In Development", CreatedAt: t0.Add(48 * time.Hour)},
			{Author: "Some QA", From: "In Development", To: "Done", CreatedAt: t0.Add(72 * time.Hour)},
		},
	}}}
	pub := &fakePublisher{}
	h := buildRNHandler(t, j, sprints, prs, newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	row := pub.calls[0].LogRows[0]
	if len(row.DevOwners) != 1 || row.DevOwners[0] != "Dev Two" {
		t.Errorf("dev owner should be the LAST author to move ticket into an 'in development' status; got %v", row.DevOwners)
	}
}

func TestReleaseNotesHandler_ForceRefresh_PropagatesToPublisher(t *testing.T) {
	// Operator can trigger a backfill of existing rows by sending
	// ?force=true. The enqueuer packs Force into the payload; the
	// handler must pass it through as publish.Input.ForceRefresh.
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	j := &fakeJira{issues: []apijira.Issue{{Key: "SIS-1", Summary: "s", Assignee: "alice"}}}
	pub := &fakePublisher{}

	h := buildRNHandler(t, j, sprints, prs, newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, pub, 0)

	payload, _ := json.Marshal(ReleaseNotesRequestPayload{
		SprintID: sp.ID().UUID(),
		Reason:   "operator",
		Force:    true,
	})
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventReleaseNotesRequested),
		Payload:   payload,
	}
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if pub.called != 1 {
		t.Fatalf("publisher should have run once; called=%d", pub.called)
	}
	if !pub.calls[0].ForceRefresh {
		t.Errorf("publisher input should carry ForceRefresh=true when payload.Force is set")
	}
}

func TestReleaseNotesHandler_HappyPath_SavesAndPublishes(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	repoID := shared.NewRepositoryID()
	prs := newFakePRs()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	releases := newFakeReleases()
	an := &fakeAnalyses{}
	comp := &fakeComposer{notes: stubNotes(pr.ID())}
	pub := &fakePublisher{}

	h := buildRNHandler(t, &fakeJira{keys: []string{"SIS-1"}}, sprints, prs, releases, an, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if comp.called != 1 {
		t.Errorf("composer calls: got %d want 1", comp.called)
	}
	if pub.called != 1 {
		t.Errorf("publisher calls: got %d want 1", pub.called)
	}
	if len(an.saved) != 1 {
		t.Errorf("analyses saved: got %d want 1", len(an.saved))
	}
	if len(releases.saved) != 2 {
		// 1 = draft, 2 = mark published
		t.Errorf("releases saved: got %d want 2", len(releases.saved))
	}
	// Final state must be Published.
	final := releases.saved[len(releases.saved)-1]
	if !final.IsPublished() {
		t.Errorf("final release status: got %q want published", final.Status())
	}
	if final.ContentHash() == "" {
		t.Error("final release ContentHash is empty")
	}
	if final.Version().String() != "v0.0.1" {
		t.Errorf("version: got %q want v0.0.1", final.Version().String())
	}
	// Publisher input shape.
	pi := pub.calls[0]
	if pi.PageID != "123456" {
		t.Errorf("PageID: got %q want 123456", pi.PageID)
	}
	// Handler now publishes table-only: LogRows set, Notes empty.
	if len(pi.LogRows) == 0 {
		t.Error("LogRows is empty in publisher input (table-only publish expected)")
	} else {
		r0 := pi.LogRows[0]
		if r0.TicketKey == "" {
			t.Error("LogRows[0].TicketKey is empty")
		}
		if r0.SprintJiraID == 0 {
			t.Error("LogRows[0].SprintJiraID is zero")
		}
	}
	if pi.Notes != nil {
		t.Error("Notes should be nil (handler emits table-only output)")
	}
}

func TestReleaseNotesHandler_L3_SkipsWhenHashUnchangedAndRecent(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	repoID := shared.NewRepositoryID()
	prs := newFakePRs()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	// First run -> save + publish.
	releases := newFakeReleases()
	an := &fakeAnalyses{}
	comp := &fakeComposer{notes: stubNotes(pr.ID())}
	pub := &fakePublisher{}
	h := buildRNHandler(t, &fakeJira{keys: []string{"SIS-1"}}, sprints, prs, releases, an, comp, pub, time.Hour)
	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	wantPubCalls := pub.called

	// Second run with same inputs -> L3 short-circuit; no new LLM or publish.
	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if comp.called != 1 {
		t.Errorf("composer should NOT run on second call; called=%d want 1", comp.called)
	}
	if pub.called != wantPubCalls {
		t.Errorf("publisher should NOT run on second call; called=%d want %d", pub.called, wantPubCalls)
	}
}

func TestReleaseNotesHandler_L3_DoesNotSkipWhenHashDiffers(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	repoID := shared.NewRepositoryID()
	prs := newFakePRs()
	pr1 := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr1)

	releases := newFakeReleases()
	comp := &fakeComposer{notes: stubNotes(pr1.ID())}
	pub := &fakePublisher{}
	h := buildRNHandler(t, &fakeJira{keys: []string{"SIS-1"}}, sprints, prs, releases, &fakeAnalyses{}, comp, pub, time.Hour)
	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("first Handle: %v", err)
	}

	// A new PR lands -> new hash -> publish runs again.
	pr2 := mkMergedPR(t, repoID, 2, "beefcafe", "SIS-1")
	_ = prs.Save(context.Background(), pr2)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if comp.called != 2 {
		t.Errorf("composer calls: got %d want 2", comp.called)
	}
	if pub.called != 2 {
		t.Errorf("publisher calls: got %d want 2", pub.called)
	}
}

func TestReleaseNotesHandler_PublishFailure_ReturnsRetryableError(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	repoID := shared.NewRepositoryID()
	prs := newFakePRs()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	releases := newFakeReleases()
	comp := &fakeComposer{notes: stubNotes(pr.ID())}
	pub := &fakePublisher{err: errors.New("confluence 500")}
	h := buildRNHandler(t, &fakeJira{keys: []string{"SIS-1"}}, sprints, prs, releases, &fakeAnalyses{}, comp, pub, 0)

	err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID()))
	if err == nil {
		t.Fatal("want publish error, got nil")
	}
	// Draft must be saved even though publish failed -- a retry can
	// use the existing draft via the L3 lookup.
	if len(releases.saved) != 1 {
		t.Errorf("draft should be saved before publish; saved=%d want 1", len(releases.saved))
	}
	if releases.saved[0].IsPublished() {
		t.Error("draft should NOT be in published state when publish failed")
	}
}

func TestReleaseNotesHandler_ComposerFailure_NoSave(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	repoID := shared.NewRepositoryID()
	prs := newFakePRs()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	releases := newFakeReleases()
	comp := &fakeComposer{err: errors.New("llm timeout")}
	pub := &fakePublisher{}
	h := buildRNHandler(t, &fakeJira{keys: []string{"SIS-1"}}, sprints, prs, releases, &fakeAnalyses{}, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err == nil {
		t.Fatal("want composer error, got nil")
	}
	if len(releases.saved) != 0 {
		t.Errorf("nothing should be saved when composer fails; saved=%d", len(releases.saved))
	}
}

func TestReleaseNotesHandler_WrongEventType_ReturnsError(t *testing.T) {
	h := buildRNHandler(t, &fakeJira{}, newFakeSprints(), newFakePRs(), newFakeReleases(),
		&fakeAnalyses{}, &fakeComposer{}, &fakePublisher{}, 0)
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: "wrong.event",
		Payload:   []byte("{}"),
	}
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want error, got nil")
	}
}

func TestNextReleaseVersion_FirstIsV001(t *testing.T) {
	v, err := nextReleaseVersion(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v.String() != "v0.0.1" {
		t.Errorf("first version: got %q want v0.0.1", v.String())
	}
}

func TestNextReleaseVersion_IncrementsAcrossExisting(t *testing.T) {
	mkRel := func(version string) *release.Release {
		v, _ := release.NewVersion(version)
		r, _ := release.Draft(release.DraftInput{
			SprintID: shared.NewSprintID(), Version: v, ContentHash: "x",
		}, shared.SystemClock{})
		return r
	}
	v, err := nextReleaseVersion([]*release.Release{
		mkRel("v0.0.1"), mkRel("v0.0.3"), mkRel("v0.0.2"),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v.String() != "v0.0.4" {
		t.Errorf("got %q want v0.0.4", v.String())
	}
}

// ---- helper-function unit tests for the M10.5 cosmetic fixes -------

func TestRewriteTicketLinks_EmptyBase_NoChange(t *testing.T) {
	in := "see [SIS-10401](https://jira.example.com/browse/SIS-10401) for details"
	if got := rewriteTicketLinks(in, ""); got != in {
		t.Errorf("empty base must be a no-op\n got: %q\nwant: %q", got, in)
	}
}

func TestRewriteTicketLinks_RewritesLLMPlaceholder(t *testing.T) {
	in := "fix landed: [SIS-10401](https://jira.example.com/browse/SIS-10401)"
	want := "fix landed: [SIS-10401](https://shl.atlassian.net/browse/SIS-10401)"
	if got := rewriteTicketLinks(in, "https://shl.atlassian.net"); got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestRewriteTicketLinks_TrimsTrailingSlash(t *testing.T) {
	in := "[ABC-1](https://bad/browse/ABC-1)"
	want := "[ABC-1](https://shl.atlassian.net/browse/ABC-1)"
	if got := rewriteTicketLinks(in, "https://shl.atlassian.net///"); got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestRewriteTicketLinks_MultipleLinks(t *testing.T) {
	in := "- [SIS-10401](https://a/x)\n- [SIS-10402](https://b/y)\n- non-key [Foo](https://c)"
	got := rewriteTicketLinks(in, "https://shl.atlassian.net")
	want := "- [SIS-10401](https://shl.atlassian.net/browse/SIS-10401)\n- [SIS-10402](https://shl.atlassian.net/browse/SIS-10402)\n- non-key [Foo](https://c)"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestRewriteTicketLinks_LowercaseLabelNotRewritten(t *testing.T) {
	// Only labels matching the Jira ticket-key shape are rewritten;
	// arbitrary lowercase labels survive untouched.
	in := "[click](https://a/b)"
	if got := rewriteTicketLinks(in, "https://shl.atlassian.net"); got != in {
		t.Errorf("non-ticket label must not be rewritten\n got: %q\nwant: %q", got, in)
	}
}

func TestStartsWithPrefix(t *testing.T) {
	cases := []struct {
		name, prefix string
		want         bool
	}{
		{"Marvelous_Sprint_187", "Marvelous", true},
		{"Marvelous Sprint 187", "Marvelous", true},
		{"marvelous_sprint_187", "Marvelous", true}, // case-insensitive
		{"Marvelous", "Marvelous", true},            // exact match
		{"Kang_Sprint_5", "Marvelous", false},
		{"MarvelousX_Sprint_1", "Marvelous", false}, // no separator after prefix
		{"Mar", "Marvelous", false},                 // shorter than prefix
		{"anything", "", false},                     // empty prefix is never a prefix
	}
	for _, c := range cases {
		t.Run(c.name+"/"+c.prefix, func(t *testing.T) {
			if got := startsWithPrefix(c.name, c.prefix); got != c.want {
				t.Errorf("startsWithPrefix(%q, %q) = %v, want %v", c.name, c.prefix, got, c.want)
			}
		})
	}
}

func TestConfluenceHeading_FoldsDuplicatedPrefix(t *testing.T) {
	end := time.Date(2026, 6, 16, 16, 30, 0, 0, time.UTC)
	sp := buildClosedSprint(t, "Marvelous", "Marvelous_Sprint_185", end)
	got := confluenceHeading("Marvelous", sp)
	want := "Marvelous_Sprint_185 \u2014 closed 2026-06-16"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestConfluenceHeading_PrependsWhenSprintNameLacksPrefix(t *testing.T) {
	end := time.Date(2026, 6, 16, 16, 30, 0, 0, time.UTC)
	sp := buildClosedSprint(t, "Marvelous", "Sprint_185", end)
	got := confluenceHeading("Marvelous", sp)
	want := "Marvelous Sprint_185 \u2014 closed 2026-06-16"
	if got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

// buildClosedSprint is a minimal helper for the heading tests. It
// only needs to expose Name(), Team(), State(), EndsAt() so we can
// construct via sprint.Rehydrate.
func buildClosedSprint(t *testing.T, team, name string, end time.Time) *sprint.Sprint {
	t.Helper()
	return sprint.Rehydrate(
		shared.NewSprintID(),
		3974, 187,
		team, name, "",
		sprint.StateClosed,
		nil, &end, &end,
		end,
	)
}

// ---- M10.5 helper unit tests ---------------------------------------

func TestSprintNumberFromName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"Marvelous_Sprint_185", 185},
		{"Marvelous Sprint 187", 187},
		{"Sprint_1", 1},
		{"185", 185},                   // bare number
		{"Marvelous_Sprint_185 ", 185}, // trailing space tolerated
		{"Marvelous", 0},               // no trailing digits
		{"", 0},                        // empty
		{"Sprint_185_v2", 2},           // captures only the trailing number group
		{"Sprint_185_2026", 2026},      // year wins because it is trailing
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sprintNumberFromName(c.name); got != c.want {
				t.Errorf("sprintNumberFromName(%q) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// fakeSummariser captures inputs and returns a canned reply / error.
type fakeSummariser struct {
	mu    sync.Mutex
	calls []ticketsummary.Input
	reply string
	err   error
}

func (f *fakeSummariser) Summarise(_ context.Context, in ticketsummary.Input) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.reply, f.err
}

func TestReleaseNotesHandler_Summariser_HappyPath_PopulatesDescription(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	repoID := shared.NewRepositoryID()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	pub := &fakePublisher{}
	summ := &fakeSummariser{reply: "Fixed the search filter behaviour."}

	stores := persistence.Stores{
		Sprints: sprints, PullRequests: prs, Releases: newFakeReleases(), Analyses: &fakeAnalyses{},
	}
	h, err := NewReleaseNotesHandler(ReleaseNotesHandlerDeps{
		Jira:             &fakeJira{keys: []string{"SIS-1"}},
		Stores:           stores,
		UoW:              &trivialUoW{s: stores},
		Summariser:       summ,
		Publisher:        pub,
		ConfluencePageID: "x",
		HeadingPrefix:    "Marvelous",
		Logger:           discardLogger(),
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(summ.calls) != 1 {
		t.Fatalf("summariser calls: got %d want 1", len(summ.calls))
	}
	if summ.calls[0].TicketKey != "SIS-1" {
		t.Errorf("summariser ticket: got %q want SIS-1", summ.calls[0].TicketKey)
	}
	if len(pub.calls) != 1 || len(pub.calls[0].LogRows) != 1 {
		t.Fatalf("publisher should have 1 row; got %+v", pub.calls)
	}
	if pub.calls[0].LogRows[0].Description != "Fixed the search filter behaviour." {
		t.Errorf("description: got %q want LLM reply", pub.calls[0].LogRows[0].Description)
	}
}

func TestReleaseNotesHandler_Summariser_Failure_FallsBackToJiraSummary(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	prs := newFakePRs()
	repoID := shared.NewRepositoryID()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	pub := &fakePublisher{}
	summ := &fakeSummariser{err: errors.New("LLM down")}

	stores := persistence.Stores{
		Sprints: sprints, PullRequests: prs, Releases: newFakeReleases(), Analyses: &fakeAnalyses{},
	}
	h, _ := NewReleaseNotesHandler(ReleaseNotesHandlerDeps{
		Jira:             &fakeJira{keys: []string{"SIS-1"}}, // stub summary="stub"
		Stores:           stores,
		UoW:              &trivialUoW{s: stores},
		Summariser:       summ,
		Publisher:        pub,
		ConfluencePageID: "x",
		HeadingPrefix:    "Marvelous",
		Logger:           discardLogger(),
	})
	// Handler must NOT fail even when the summariser errors.
	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle returned error -- summariser failure should be tolerated: %v", err)
	}
	// M11: on summariser error, description now falls back to Jira's
	// summary rather than blank. Empty cells only when Jira also has
	// nothing (which the fake never produces here).
	if got := pub.calls[0].LogRows[0].Description; got != "stub" {
		t.Errorf("description should fall back to Jira summary on summariser error; got %q", got)
	}
}

// TestReleaseNotesHandler_UnlinkedPRs_SurfaceOnPublish verifies that
// merged PRs whose head branch didn't carry a Jira ticket key are
// projected onto publish.Input.UnlinkedPRs so operators can spot them
// on the Confluence release-notes page. This is the observability
// signal the sprint tracker relies on.
func TestReleaseNotesHandler_UnlinkedPRs_SurfaceOnPublish(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	// Seed a repository so buildUnlinkedPRRefs can resolve the full name
	// (owner/name) via Stores.Repos rather than the placeholder path.
	repos := newFakeRepos()
	repo, err := repository.Register(repository.RegistrationInput{Owner: "acme", Name: "svc"}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("Register repo: %v", err)
	}
	_ = repos.Save(context.Background(), repo)

	// Seed an ORPHAN merged PR: merged in the sprint window, zero tickets.
	// The fake's ListMergedWithoutTicketsInWindow implementation walks
	// stored PRs and applies both filters.
	prs := newFakePRs()
	author, _ := shared.NewAuthor("alice", "Alice", "a@example.com")
	src, _ := shared.NewBranch("hotfix/nightly")
	dst, _ := shared.NewBranch("main")
	orphan, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: repo.ID(),
		Number:       913,
		Title:        "hotfix: bump timeout",
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("Open PR: %v", err)
	}
	sha, _ := shared.NewSHA("cafef00d")
	// Merge time must fall inside the sprint window built by mkSprint
	// (which is [now-14d, now]); "yesterday" is safely inside.
	mergedAt := time.Now().Add(-24 * time.Hour)
	if err := orphan.Merge(mergedAt, sha); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_ = prs.Save(context.Background(), orphan)

	// Also seed a normally-linked ticket so the handler follows the
	// tickets-only publish path (no composer required) and we can
	// assert UnlinkedPRs are attached alongside LogRows.
	pub := &fakePublisher{}
	j := &fakeJira{issues: []apijira.Issue{{
		Key: "SIS-1", Summary: "ticket summary", Assignee: "alice",
	}}}

	// Custom wiring: buildRNHandler does not accept a Repos store, so
	// build the handler directly.
	stores := persistence.Stores{
		Sprints:      sprints,
		PullRequests: prs,
		Releases:     newFakeReleases(),
		Analyses:     &fakeAnalyses{},
		Repos:        repos,
	}
	h, err := NewReleaseNotesHandler(ReleaseNotesHandlerDeps{
		Jira:             j,
		Stores:           stores,
		UoW:              &trivialUoW{s: stores},
		Composer:         &fakeComposer{},
		Publisher:        pub,
		ConfluencePageID: "123456",
		HeadingPrefix:    "Marvelous",
		Logger:           discardLogger(),
		Clock:            shared.SystemClock{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if pub.called != 1 {
		t.Fatalf("publisher calls: got %d want 1", pub.called)
	}
	got := pub.calls[0].UnlinkedPRs
	if len(got) != 1 {
		t.Fatalf("UnlinkedPRs: got %d want 1", len(got))
	}
	row := got[0]
	if row.RepoFullName != "acme/svc" {
		t.Errorf("RepoFullName: got %q want acme/svc", row.RepoFullName)
	}
	if row.Number != 913 {
		t.Errorf("Number: got %d want 913", row.Number)
	}
	if row.HeadBranch != "hotfix/nightly" {
		t.Errorf("HeadBranch: got %q want hotfix/nightly", row.HeadBranch)
	}
	if row.Author != "alice" {
		t.Errorf("Author: got %q want alice", row.Author)
	}
	if row.URL != "https://github.com/acme/svc/pull/913" {
		t.Errorf("URL: got %q", row.URL)
	}
	if row.MergedAt.IsZero() {
		t.Error("MergedAt must not be zero")
	}
}

// TestReleaseNotesHandler_UnlinkedPRs_EmptyWhenAllLinked verifies that
// when no orphans exist the handler passes an empty UnlinkedPRs slice
// (so the publisher clears any stale section).
func TestReleaseNotesHandler_UnlinkedPRs_EmptyWhenAllLinked(t *testing.T) {
	sprints := newFakeSprints()
	sp := mkSprint(t, 187, sprint.StateClosed)
	_ = sprints.Save(context.Background(), sp)

	// Every merged PR is properly linked -> no orphans.
	prs := newFakePRs()
	repoID := shared.NewRepositoryID()
	pr := mkMergedPR(t, repoID, 1, "deadbeef", "SIS-1")
	_ = prs.Save(context.Background(), pr)

	pub := &fakePublisher{}
	j := &fakeJira{issues: []apijira.Issue{{Key: "SIS-1", Summary: "s", Assignee: "alice"}}}
	comp := &fakeComposer{notes: stubNotes(pr.ID())}
	h := buildRNHandler(t, j, sprints, prs, newFakeReleases(),
		&fakeAnalyses{}, comp, pub, 0)

	if err := h.Handle(context.Background(), mkRNEvent(t, sp.ID().UUID())); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if pub.called != 1 {
		t.Fatalf("publisher calls: got %d want 1", pub.called)
	}
	if got := pub.calls[0].UnlinkedPRs; len(got) != 0 {
		t.Errorf("UnlinkedPRs should be empty when all merged PRs are linked; got %d rows", len(got))
	}
}
