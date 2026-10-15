package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	apigithub "github.com/repomind/repomind/internal/application/github"
	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/repository"
	"github.com/repomind/repomind/internal/domain/shared"
)

// ---------- fake GitHub client ----------

type fakeGitHub struct {
	pr      *apigithub.PullRequest
	files   []apigithub.File
	commits []apigithub.Commit
	reviews []apigithub.Review

	getErr     error
	filesErr   error
	commitsErr error
	reviewsErr error
}

func (f *fakeGitHub) ListPullRequests(context.Context, apigithub.RepositoryRef, apigithub.ListPROptions) (*apigithub.PullRequestPage, error) {
	return nil, nil
}
func (f *fakeGitHub) GetPullRequest(context.Context, apigithub.RepositoryRef, int) (*apigithub.PullRequest, error) {
	return f.pr, f.getErr
}
func (f *fakeGitHub) ListPullRequestFiles(context.Context, apigithub.RepositoryRef, int, apigithub.PageOptions) (*apigithub.FilePage, error) {
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	return &apigithub.FilePage{Items: f.files}, nil
}
func (f *fakeGitHub) ListPullRequestCommits(context.Context, apigithub.RepositoryRef, int, apigithub.PageOptions) (*apigithub.CommitPage, error) {
	if f.commitsErr != nil {
		return nil, f.commitsErr
	}
	return &apigithub.CommitPage{Items: f.commits}, nil
}
func (f *fakeGitHub) ListPullRequestReviews(context.Context, apigithub.RepositoryRef, int, apigithub.PageOptions) (*apigithub.ReviewPage, error) {
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return &apigithub.ReviewPage{Items: f.reviews}, nil
}

// validGitHubPR returns a minimal valid PR DTO for the fake.
func validGitHubPR() *apigithub.PullRequest {
	return &apigithub.PullRequest{
		Number:     42,
		Title:      "Add the widget",
		Body:       "Implements the SIS-1 widget per the design doc.",
		State:      apigithub.PRStateOpen,
		BaseBranch: "main",
		HeadBranch: "feature/widget",
		Author:     apigithub.UserRef{Login: "octocat", Name: "Octo Cat"},
		Labels:     []string{"enhancement"},
		CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt:  time.Unix(1_700_000_100, 0).UTC(),
		URL:        "https://github.com/acme/thing/pull/42",
	}
}

// ---------- fake stores ----------

type fakeRepos struct {
	mu     sync.Mutex
	byName map[string]*repository.Repository
	saved  []*repository.Repository
	getErr error
}

func newFakeRepos() *fakeRepos                     { return &fakeRepos{byName: make(map[string]*repository.Repository)} }
func (r *fakeRepos) key(owner, name string) string { return owner + "/" + name }

func (r *fakeRepos) Save(_ context.Context, repo *repository.Repository) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[r.key(repo.Owner(), repo.Name())] = repo
	r.saved = append(r.saved, repo)
	return nil
}
func (r *fakeRepos) FindByID(_ context.Context, id shared.RepositoryID) (*repository.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, repo := range r.byName {
		if repo.ID() == id {
			return repo, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (r *fakeRepos) FindByFullName(_ context.Context, owner, name string) (*repository.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	repo, ok := r.byName[r.key(owner, name)]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return repo, nil
}

type fakePRs struct {
	mu      sync.Mutex
	byKey   map[string]*pullrequest.PullRequest
	saved   []*pullrequest.PullRequest
	saveErr error
}

func newFakePRs() *fakePRs { return &fakePRs{byKey: make(map[string]*pullrequest.PullRequest)} }
func (p *fakePRs) key(repoID shared.RepositoryID, n pullrequest.Number) string {
	return repoID.String() + "#" + strconv.Itoa(int(n))
}
func (p *fakePRs) Save(_ context.Context, pr *pullrequest.PullRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saveErr != nil {
		return p.saveErr
	}
	p.byKey[p.key(pr.RepositoryID(), pr.Number())] = pr
	p.saved = append(p.saved, pr)
	return nil
}
func (p *fakePRs) FindByID(context.Context, shared.PullRequestID) (*pullrequest.PullRequest, error) {
	return nil, persistence.ErrNotFound
}
func (p *fakePRs) FindByRepoNumber(_ context.Context, repoID shared.RepositoryID, n pullrequest.Number) (*pullrequest.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr, ok := p.byKey[p.key(repoID, n)]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return pr, nil
}
func (p *fakePRs) ListByTickets(_ context.Context, tickets []shared.TicketKey) ([]*pullrequest.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(tickets) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{}, len(tickets))
	for _, t := range tickets {
		want[t.String()] = struct{}{}
	}
	var out []*pullrequest.PullRequest
	for _, pr := range p.byKey {
		for _, t := range pr.Tickets() {
			if _, ok := want[t.String()]; ok {
				out = append(out, pr)
				break
			}
		}
	}
	return out, nil
}

func (p *fakePRs) ListMergedWithoutTicketsInWindow(_ context.Context, start, end time.Time) ([]*pullrequest.PullRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil, nil
	}
	var out []*pullrequest.PullRequest
	for _, pr := range p.byKey {
		if !pr.IsMerged() {
			continue
		}
		m := pr.MergedAt()
		if m == nil {
			continue
		}
		if m.Before(start) || m.After(end) {
			continue
		}
		if len(pr.Tickets()) > 0 {
			continue
		}
		out = append(out, pr)
	}
	return out, nil
}

type fakeAnalyses struct {
	mu      sync.Mutex
	saved   []*analysis.AnalysisResult
	saveErr error
}

func (a *fakeAnalyses) Save(_ context.Context, r *analysis.AnalysisResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.saveErr != nil {
		return a.saveErr
	}
	a.saved = append(a.saved, r)
	return nil
}
func (a *fakeAnalyses) FindByID(context.Context, shared.AnalysisID) (*analysis.AnalysisResult, error) {
	return nil, persistence.ErrNotFound
}
func (a *fakeAnalyses) ListByPullRequest(context.Context, shared.PullRequestID, analysis.Kind) ([]*analysis.AnalysisResult, error) {
	return nil, nil
}
func (a *fakeAnalyses) ListByRelease(context.Context, shared.ReleaseID, analysis.Kind) ([]*analysis.AnalysisResult, error) {
	return nil, nil
}

// trivialUoW invokes fn with the wrapped Stores (no real tx; sufficient
// for unit tests of the handler's orchestration logic).
type trivialUoW struct{ s persistence.Stores }

func (u *trivialUoW) Within(ctx context.Context, fn func(context.Context, persistence.Stores) error) error {
	return fn(ctx, u.s)
}

// ---------- fake analysers ----------

type fakeAnalyser struct {
	kind   analysis.Kind
	err    error
	mu     sync.Mutex
	called int
}

func (f *fakeAnalyser) Analyze(_ context.Context, pr *pullrequest.PullRequest) (*analysis.AnalysisResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	// Build a minimal valid AnalysisResult for the requested kind.
	return analysis.NewResult(
		f.kind,
		analysis.SubjectForPR(pr.ID()),
		analysis.AnalyzerRef{Name: string(f.kind), Version: "1.0.0", PromptVersion: "1.0.0"},
		stubPayload(f.kind),
		"gpt-test",
		42,
		shared.SystemClock{},
	)
}

func stubPayload(k analysis.Kind) analysis.Payload {
	switch k {
	case analysis.KindRisk:
		r, _ := analysis.NewRiskReport(analysis.RiskLow, nil, "no concerns identified", 0.8)
		return r
	case analysis.KindQARecommendations:
		q, _ := analysis.NewQARecommendations(
			"smoke + integration",
			nil, nil,
			[]analysis.TestScenario{{Title: "happy path", Priority: analysis.PriorityP1}},
		)
		return q
	default:
		return nil
	}
}

// ---------- helpers ----------

// buildHandler wires the fakes into a working PRAnalysisHandler.
func buildHandler(t *testing.T, gh *fakeGitHub, repos *fakeRepos, prs *fakePRs, an *fakeAnalyses, risk, qa *fakeAnalyser) *PRAnalysisHandler {
	t.Helper()
	stores := persistence.Stores{
		Repos:        repos,
		PullRequests: prs,
		Analyses:     an,
	}
	h, err := NewPRAnalysisHandler(PRAnalysisHandlerDeps{
		GitHub:       gh,
		Stores:       stores,
		UoW:          &trivialUoW{s: stores},
		RiskAnalyser: risk,
		QAAnalyser:   qa,
		Logger:       discardLogger(),
		Clock:        shared.SystemClock{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	return h
}

func mkPRAnalysisEvent(t *testing.T, owner, name string, num int, sha string) persistence.OutboxEvent {
	t.Helper()
	payload, err := json.Marshal(PRAnalysisRequestPayload{
		Repo:   RepoRef{Owner: owner, Name: name},
		Number: num, HeadSHA: sha,
		Action: "opened", DeliveryID: "delivery-x",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: string(EventPRAnalysisRequested),
		Payload:   payload,
	}
}

// ---------- tests ----------

func TestNewPRAnalysisHandler_RejectsNilDeps(t *testing.T) {
	gh := &fakeGitHub{pr: validGitHubPR()}
	stores := persistence.Stores{Repos: newFakeRepos(), PullRequests: newFakePRs(), Analyses: &fakeAnalyses{}}
	uow := &trivialUoW{s: stores}
	rsk := &fakeAnalyser{kind: analysis.KindRisk}
	q := &fakeAnalyser{kind: analysis.KindQARecommendations}
	lg := discardLogger()

	bad := []PRAnalysisHandlerDeps{
		{Stores: stores, UoW: uow, RiskAnalyser: rsk, QAAnalyser: q, Logger: lg},   // nil GitHub
		{GitHub: gh, UoW: uow, RiskAnalyser: rsk, QAAnalyser: q, Logger: lg},       // nil stores
		{GitHub: gh, Stores: stores, RiskAnalyser: rsk, QAAnalyser: q, Logger: lg}, // nil uow
		{GitHub: gh, Stores: stores, UoW: uow, QAAnalyser: q, Logger: lg},          // nil risk
		{GitHub: gh, Stores: stores, UoW: uow, RiskAnalyser: rsk, Logger: lg},      // nil qa
		{GitHub: gh, Stores: stores, UoW: uow, RiskAnalyser: rsk, QAAnalyser: q},   // nil logger
	}
	for i, d := range bad {
		if _, err := NewPRAnalysisHandler(d); err == nil {
			t.Errorf("case %d: want error, got nil", i)
		}
	}
}

func TestPRAnalysisHandler_HappyPath_NewPR(t *testing.T) {
	gh := &fakeGitHub{pr: validGitHubPR()}
	repos := newFakeRepos()
	prs := newFakePRs()
	an := &fakeAnalyses{}
	risk := &fakeAnalyser{kind: analysis.KindRisk}
	qa := &fakeAnalyser{kind: analysis.KindQARecommendations}
	h := buildHandler(t, gh, repos, prs, an, risk, qa)

	evt := mkPRAnalysisEvent(t, "acme", "thing", 42, "deadbeef")
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(repos.saved) != 1 {
		t.Errorf("repos.Save calls: got %d want 1", len(repos.saved))
	}
	if len(prs.saved) != 1 {
		t.Errorf("prs.Save calls: got %d want 1", len(prs.saved))
	}
	if len(an.saved) != 2 {
		t.Errorf("analyses.Save calls: got %d want 2", len(an.saved))
	}
	if risk.called != 1 {
		t.Errorf("risk analyser calls: got %d want 1", risk.called)
	}
	if qa.called != 1 {
		t.Errorf("qa analyser calls: got %d want 1", qa.called)
	}
}

func TestPRAnalysisHandler_RiskFails_QAStillSaved(t *testing.T) {
	gh := &fakeGitHub{pr: validGitHubPR()}
	repos := newFakeRepos()
	prs := newFakePRs()
	an := &fakeAnalyses{}
	risk := &fakeAnalyser{kind: analysis.KindRisk, err: errors.New("llm timeout")}
	qa := &fakeAnalyser{kind: analysis.KindQARecommendations}
	h := buildHandler(t, gh, repos, prs, an, risk, qa)

	evt := mkPRAnalysisEvent(t, "acme", "thing", 1, "sha1")
	err := h.Handle(context.Background(), evt)
	if err == nil {
		t.Fatal("want error from failed analyser, got nil")
	}
	if qa.called != 1 {
		t.Errorf("QA should still run; called=%d want 1", qa.called)
	}
	if len(an.saved) != 1 {
		t.Errorf("analyses.Save: got %d want 1 (only QA succeeded)", len(an.saved))
	}
}

func TestPRAnalysisHandler_QAFails_RiskStillSaved(t *testing.T) {
	gh := &fakeGitHub{pr: validGitHubPR()}
	repos := newFakeRepos()
	prs := newFakePRs()
	an := &fakeAnalyses{}
	risk := &fakeAnalyser{kind: analysis.KindRisk}
	qa := &fakeAnalyser{kind: analysis.KindQARecommendations, err: errors.New("llm timeout")}
	h := buildHandler(t, gh, repos, prs, an, risk, qa)

	evt := mkPRAnalysisEvent(t, "acme", "thing", 1, "sha1")
	err := h.Handle(context.Background(), evt)
	if err == nil {
		t.Fatal("want error from failed analyser, got nil")
	}
	if risk.called != 1 {
		t.Errorf("Risk should still run; called=%d", risk.called)
	}
	if len(an.saved) != 1 {
		t.Errorf("analyses.Save: got %d want 1 (only risk succeeded)", len(an.saved))
	}
}

func TestPRAnalysisHandler_GitHubFetchFails_PropagatesError(t *testing.T) {
	gh := &fakeGitHub{getErr: errors.New("github 500")}
	h := buildHandler(t, gh, newFakeRepos(), newFakePRs(), &fakeAnalyses{},
		&fakeAnalyser{kind: analysis.KindRisk}, &fakeAnalyser{kind: analysis.KindQARecommendations})
	evt := mkPRAnalysisEvent(t, "acme", "thing", 1, "sha1")
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want error, got nil")
	}
}

func TestPRAnalysisHandler_WrongEventType_ReturnsError(t *testing.T) {
	h := buildHandler(t, &fakeGitHub{pr: validGitHubPR()}, newFakeRepos(), newFakePRs(), &fakeAnalyses{},
		&fakeAnalyser{kind: analysis.KindRisk}, &fakeAnalyser{kind: analysis.KindQARecommendations})
	evt := persistence.OutboxEvent{
		ID:        uuid.New(),
		EventType: "wrong.event",
		Payload:   []byte("{}"),
	}
	if err := h.Handle(context.Background(), evt); err == nil {
		t.Error("want error, got nil")
	}
}

func TestPRAnalysisHandler_ExistingPR_Updates(t *testing.T) {
	// Pre-seed an existing PR for the same (repo, number).
	repos := newFakeRepos()
	prs := newFakePRs()
	pre, err := repository.Register(repository.RegistrationInput{Owner: "acme", Name: "thing"}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	_ = repos.Save(context.Background(), pre)

	author, _ := shared.NewAuthor("octocat", "Octo Cat", "octo@example.com")
	src, _ := shared.NewBranch("feature/widget")
	dst, _ := shared.NewBranch("main")
	existing, err := pullrequest.Open(pullrequest.OpenInput{
		RepositoryID: pre.ID(),
		Number:       42,
		Title:        "old title",
		Description:  "old desc",
		Author:       author,
		SourceBranch: src,
		TargetBranch: dst,
	}, shared.SystemClock{})
	if err != nil {
		t.Fatalf("seed pr: %v", err)
	}
	_ = prs.Save(context.Background(), existing)

	gh := &fakeGitHub{pr: validGitHubPR()} // GH still returns title="Add the widget"
	an := &fakeAnalyses{}
	h := buildHandler(t, gh, repos, prs, an,
		&fakeAnalyser{kind: analysis.KindRisk},
		&fakeAnalyser{kind: analysis.KindQARecommendations})

	evt := mkPRAnalysisEvent(t, "acme", "thing", 42, "newsha")
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Only ONE repo write (the seed); handler should have found existing.
	if len(repos.saved) != 1 {
		t.Errorf("repos.Save calls: got %d want 1 (seed only, no new register)", len(repos.saved))
	}
	// PR was updated -- saved twice (seed + update); ID preserved.
	if len(prs.saved) != 2 {
		t.Errorf("prs.Save calls: got %d want 2", len(prs.saved))
	}
	if prs.saved[0].ID() != prs.saved[1].ID() {
		t.Errorf("PR id should be preserved across update; got %s vs %s",
			prs.saved[0].ID(), prs.saved[1].ID())
	}
	if len(an.saved) != 2 {
		t.Errorf("analyses.Save calls: got %d want 2", len(an.saved))
	}
}

// TestPRAnalysisHandler_AttachesTicketKeysFromBranchAndBody verifies
// the fix for the release-notes join bug: ingested PRs MUST have
// their Jira ticket keys populated (via extractTicketKeys), otherwise
// release_notes_handler's PullRequests.ListByTickets query returns
// nothing and the Confluence PR column stays empty forever.
//
// Scenario mirrors real SHL usage: the PR body may not mention the
// ticket but the feature/bugfix branch always does.
func TestPRAnalysisHandler_AttachesTicketKeysFromBranchAndBody(t *testing.T) {
	gh := &fakeGitHub{pr: &apigithub.PullRequest{
		Number:     101,
		Title:      "Fix report export",              // no key here
		Body:       "See ticket description.",        // no key here
		HeadBranch: "feature/SIS-9505-report-export", // key ONLY here
		BaseBranch: "qa",
		State:      apigithub.PRStateOpen,
		Author:     apigithub.UserRef{Login: "puneet", Name: "Puneet Kumar"},
		CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt:  time.Unix(1_700_000_100, 0).UTC(),
		URL:        "https://github.com/acme/thing/pull/101",
	}}
	repos := newFakeRepos()
	prs := newFakePRs()
	h := buildHandler(t, gh, repos, prs, &fakeAnalyses{},
		&fakeAnalyser{kind: analysis.KindRisk},
		&fakeAnalyser{kind: analysis.KindQARecommendations})

	evt := mkPRAnalysisEvent(t, "acme", "thing", 101, "abc1234")
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(prs.saved) != 1 {
		t.Fatalf("prs.Save calls: got %d want 1", len(prs.saved))
	}
	saved := prs.saved[0]
	tickets := saved.Tickets()
	if len(tickets) != 1 {
		t.Fatalf("want 1 ticket associated (SIS-9505 from head branch); got %d: %v",
			len(tickets), tickets)
	}
	if tickets[0].String() != "SIS-9505" {
		t.Errorf("want SIS-9505; got %s", tickets[0].String())
	}
}

// TestPRAnalysisHandler_AttachesMultipleTicketsFromAllSources exercises
// the priority order: title first, then body, then head branch, then
// commit messages. Order matters for downstream hashing determinism.
func TestPRAnalysisHandler_AttachesMultipleTicketsFromAllSources(t *testing.T) {
	commitTS := time.Unix(1_700_000_050, 0).UTC()
	gh := &fakeGitHub{
		pr: &apigithub.PullRequest{
			Number:     202,
			Title:      "MFS-100: schema tweak",             // key #1
			Body:       "Related to SIS-9505 and SIS-9506.", // #2, #3
			HeadBranch: "feature/TDM-200-migration",         // #4
			BaseBranch: "qa",
			State:      apigithub.PRStateOpen,
			Author:     apigithub.UserRef{Login: "dev1"},
			CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
			UpdatedAt:  time.Unix(1_700_000_100, 0).UTC(),
			URL:        "https://github.com/acme/thing/pull/202",
		},
		commits: []apigithub.Commit{{
			SHA:       "deadbeef01",
			Message:   "MFS-100 also touches MFS-101 (dup source)",
			Author:    apigithub.UserRef{Login: "dev1"},
			Committer: apigithub.UserRef{Login: "dev1"},
			Timestamp: commitTS,
		}},
	}
	prs := newFakePRs()
	h := buildHandler(t, gh, newFakeRepos(), prs, &fakeAnalyses{},
		&fakeAnalyser{kind: analysis.KindRisk},
		&fakeAnalyser{kind: analysis.KindQARecommendations})

	evt := mkPRAnalysisEvent(t, "acme", "thing", 202, "sha202")
	if err := h.Handle(context.Background(), evt); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	saved := prs.saved[0]
	got := keysToStrings(saved.Tickets())
	want := []string{"MFS-100", "SIS-9505", "SIS-9506", "TDM-200", "MFS-101"}
	if len(got) != len(want) {
		t.Fatalf("ticket count: want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ticket[%d]: want %q got %q (full: %v)", i, want[i], got[i], got)
		}
	}
}
