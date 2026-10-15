// Package releasenotes is the application-layer use case that composes
// a sprint's release notes from the set of pull requests that landed
// during the sprint, and returns an AnalysisResult of
// analysis.KindReleaseNotes.
//
// Unlike the per-PR analyzers in sibling packages (risk, qa), this
// analyzer operates at sprint scope: input is a Sprint aggregate plus
// the list of merged PullRequests that belong to it, and the
// AnalysisResult's Subject is the Sprint.
//
// The package depends only on the application-layer llm.Provider port,
// the domain (sprint, pullrequest, analysis, shared) and the standard
// library. It is wired with a concrete adapter and the matching
// JSON-schema spec at the composition root in cmd/repomind.
package releasenotes

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// Provenance constants surfaced on analysis.AnalyzerRef.
const (
	AnalyzerName    = "release_notes"
	AnalyzerVersion = "1.0.0"
	PromptVersion   = "1.0.0"
)

// Config wires the analyzer's dependencies. Provider and Schema are
// required; Clock defaults to shared.SystemClock when nil.
//
// Temperature is passed straight through to the provider. Default 0
// (Go zero-value) keeps re-runs reproducible so reviewers can diff a
// fresh render against the original. Bump it if generated prose feels
// stale; negative values fall back to the provider's own default
// sampling.
type Config struct {
	Provider        llm.Provider
	Schema          *llm.StructuredOutputSpec
	Clock           shared.Clock
	MaxOutputTokens int
	Temperature     float64
}

// Input bundles the sprint-scope context the analyzer needs.
type Input struct {
	Sprint       *sprint.Sprint
	PullRequests []*pullrequest.PullRequest
}

// Analyzer composes release notes for a single Sprint. Instances are
// safe for concurrent use as long as the injected Provider is.
type Analyzer struct {
	cfg Config
}

// New validates cfg and constructs an Analyzer.
func New(cfg Config) (*Analyzer, error) {
	if cfg.Provider == nil {
		return nil, errors.New("releasenotes: Provider is required")
	}
	if cfg.Schema == nil || len(cfg.Schema.Schema) == 0 {
		return nil, errors.New("releasenotes: Schema is required")
	}
	if strings.TrimSpace(cfg.Schema.Name) == "" {
		return nil, errors.New("releasenotes: Schema.Name is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	return &Analyzer{cfg: cfg}, nil
}

// Analyze composes release notes for in.Sprint based on in.PullRequests
// and returns an AnalysisResult of analysis.KindReleaseNotes that is
// ready to persist.
//
// Validation:
//   - in.Sprint must be non-nil.
//   - in.PullRequests must contain at least one non-nil PR. Nil entries
//     in the slice are a programmer error and rejected.
//
// Filtering of which PRs to include (merged-only vs. everything, label
// filters, etc.) is the caller's responsibility — the analyzer renders
// what it is given.
//
// Error handling mirrors the risk / qa analyzers:
//   - Transport / provider failures bubble up unchanged so callers can
//     match them against the llm.Err* sentinels via errors.Is.
//   - Malformed JSON or domain-invariant violations on the decoded
//     payload are wrapped with llm.ErrInvalidResponse — the response
//     came from the model but does not conform to the schema we asked
//     for, so it is treated as an invalid response.
func (a *Analyzer) Analyze(ctx context.Context, in Input) (*analysis.AnalysisResult, error) {
	if in.Sprint == nil {
		return nil, errors.New("releasenotes: nil sprint")
	}
	if len(in.PullRequests) == 0 {
		return nil, errors.New("releasenotes: PullRequests is empty")
	}
	for i, pr := range in.PullRequests {
		if pr == nil {
			return nil, fmt.Errorf("releasenotes: PullRequests[%d] is nil", i)
		}
	}

	req := llm.Request{
		ModelRole: llm.RoleReleaseNotes,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: buildUserPrompt(in.Sprint, in.PullRequests)},
		},
		StructuredOutput: a.cfg.Schema,
		MaxOutputTokens:  a.cfg.MaxOutputTokens,
		Temperature:      a.cfg.Temperature,
	}

	resp, err := a.cfg.Provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: provider returned nil response", llm.ErrInvalidResponse)
	}

	payload, err := decodePayload([]byte(resp.Content))
	if err != nil {
		return nil, fmt.Errorf("%w: release_notes payload: %v", llm.ErrInvalidResponse, err)
	}

	result, err := analysis.NewResult(
		analysis.KindReleaseNotes,
		analysis.SubjectForSprint(in.Sprint.ID()),
		analysis.AnalyzerRef{
			Name:          AnalyzerName,
			Version:       AnalyzerVersion,
			PromptVersion: PromptVersion,
		},
		payload,
		resp.Model,
		resp.Usage.TotalTokens,
		a.cfg.Clock,
	)
	if err != nil {
		return nil, fmt.Errorf("releasenotes: build analysis result: %w", err)
	}
	return result, nil
}
