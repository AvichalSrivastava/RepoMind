// Package analysis contains the AnalysisResult aggregate and the
// value objects produced by RepoMind agents (risk assessments, release
// notes, QA recommendations).
//
// AnalysisResult is intentionally polymorphic: agents produce different
// payload shapes but the aggregate envelope, identity, traceability,
// and persistence concerns are unified.
package analysis

import (
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/shared"
)

// Kind enumerates the analysis types RepoMind produces. Adding a new
// agent in the future means adding a Kind constant — the framework
// itself stays unchanged (Module 3).
type Kind string

const (
	KindRisk              Kind = "risk"
	KindReleaseNotes      Kind = "release_notes"
	KindQARecommendations Kind = "qa_recommendations"
)

func (k Kind) Valid() bool {
	switch k {
	case KindRisk, KindReleaseNotes, KindQARecommendations:
		return true
	}
	return false
}

// Subject identifies what the analysis is *about*. We use a tagged
// union here rather than separate aggregates because every analysis
// shares the same envelope.
type Subject struct {
	PullRequestID *shared.PullRequestID
	SprintID      *shared.SprintID
	ReleaseID     *shared.ReleaseID
}

// SubjectForPR is a convenience constructor.
func SubjectForPR(id shared.PullRequestID) Subject { return Subject{PullRequestID: &id} }

// SubjectForSprint is a convenience constructor.
func SubjectForSprint(id shared.SprintID) Subject { return Subject{SprintID: &id} }

// SubjectForRelease is a convenience constructor.
func SubjectForRelease(id shared.ReleaseID) Subject { return Subject{ReleaseID: &id} }

// IsZero reports whether the Subject has no reference.
func (s Subject) IsZero() bool {
	return s.PullRequestID == nil && s.SprintID == nil && s.ReleaseID == nil
}

// Payload is the marker interface implemented by every analysis output
// value object (RiskReport, ReleaseNotes, QARecommendations, …).
type Payload interface {
	payloadKind() Kind
}

// AnalyzerRef captures provenance: which analyzer produced this result
// and with what prompt version. Used for auditability and A/B comparison.
type AnalyzerRef struct {
	Name          string
	Version       string // semver of the analyzer code
	PromptVersion string // semver of the prompt template
}

// AnalysisResult is the aggregate root for every AI-generated output.
//
// Invariants:
//   - Kind matches Payload.payloadKind().
//   - Subject is non-zero.
//   - AnalyzerRef.Name is non-empty.
type AnalysisResult struct {
	id          shared.AnalysisID
	kind        Kind
	subject     Subject
	analyzer    AnalyzerRef
	payload     Payload
	generatedAt time.Time
	model       string // LLM model identifier (e.g. "gpt-4o-mini")
	tokensUsed  int    // best-effort accounting; 0 when unknown
}

// NewResult constructs a validated AnalysisResult.
func NewResult(
	kind Kind,
	subject Subject,
	analyzer AnalyzerRef,
	payload Payload,
	model string,
	tokensUsed int,
	clock shared.Clock,
) (*AnalysisResult, error) {
	if !kind.Valid() {
		return nil, shared.NewValidationError("kind", fmt.Sprintf("invalid kind %q", kind))
	}
	if subject.IsZero() {
		return nil, shared.NewValidationError("subject", "must reference a PR, sprint, or release")
	}
	if strings.TrimSpace(analyzer.Name) == "" {
		return nil, shared.NewValidationError("analyzer.name", "must not be empty")
	}
	if payload == nil {
		return nil, shared.NewValidationError("payload", "must not be nil")
	}
	if payload.payloadKind() != kind {
		return nil, fmt.Errorf("%w: payload kind %q does not match analysis kind %q",
			shared.ErrInvariant, payload.payloadKind(), kind)
	}
	if tokensUsed < 0 {
		return nil, shared.NewValidationError("tokens_used", "must be non-negative")
	}
	return &AnalysisResult{
		id:          shared.NewAnalysisID(),
		kind:        kind,
		subject:     subject,
		analyzer:    analyzer,
		payload:     payload,
		generatedAt: clock.Now(),
		model:       strings.TrimSpace(model),
		tokensUsed:  tokensUsed,
	}, nil
}

// ---------- Accessors ----------

func (a *AnalysisResult) ID() shared.AnalysisID  { return a.id }
func (a *AnalysisResult) Kind() Kind             { return a.kind }
func (a *AnalysisResult) Subject() Subject       { return a.subject }
func (a *AnalysisResult) Analyzer() AnalyzerRef  { return a.analyzer }
func (a *AnalysisResult) Payload() Payload       { return a.payload }
func (a *AnalysisResult) GeneratedAt() time.Time { return a.generatedAt }
func (a *AnalysisResult) Model() string          { return a.model }
func (a *AnalysisResult) TokensUsed() int        { return a.tokensUsed }

// ---------- Persistence rehydration ----------

// Rehydrate reconstructs an AnalysisResult from persisted state.
// Only the persistence adapter should call this; it bypasses the
// validating constructor in favour of trusting the storage layer.
func Rehydrate(
	id shared.AnalysisID,
	kind Kind,
	subject Subject,
	analyzer AnalyzerRef,
	payload Payload,
	model string,
	tokensUsed int,
	generatedAt time.Time,
) *AnalysisResult {
	return &AnalysisResult{
		id:          id,
		kind:        kind,
		subject:     subject,
		analyzer:    analyzer,
		payload:     payload,
		generatedAt: generatedAt,
		model:       model,
		tokensUsed:  tokensUsed,
	}
}
