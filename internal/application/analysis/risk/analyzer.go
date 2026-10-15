// Package risk is the application-layer use case that classifies a
// PullRequest by deployment / regression risk and returns an
// AnalysisResult of analysis.KindRisk.
//
// The package depends only on the application-layer llm.Provider port,
// the domain (pullrequest, analysis, shared) and the standard library.
// It is wired with a concrete adapter and the matching JSON-schema
// spec at the composition root in cmd/repomind.
package risk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
)

// Provenance constants surfaced on analysis.AnalyzerRef.
const (
	AnalyzerName    = "risk"
	AnalyzerVersion = "1.0.0"
	PromptVersion   = "1.0.0"
)

// Config wires the analyzer's dependencies. Provider and Schema are
// required; Clock defaults to shared.SystemClock when nil.
//
// Temperature is passed straight through to the provider — its
// zero-value of 0.0 means "fully deterministic", which is the right
// default for a classification task. Set a negative value to fall back
// to the provider's own default sampling.
type Config struct {
	Provider        llm.Provider
	Schema          *llm.StructuredOutputSpec
	Clock           shared.Clock
	MaxOutputTokens int
	Temperature     float64
}

// Analyzer assesses a single PullRequest for deployment / regression
// risk by delegating classification to an LLM. Instances are safe for
// concurrent use as long as the injected Provider is.
type Analyzer struct {
	cfg Config
}

// New validates cfg and constructs an Analyzer.
func New(cfg Config) (*Analyzer, error) {
	if cfg.Provider == nil {
		return nil, errors.New("risk: Provider is required")
	}
	if cfg.Schema == nil || len(cfg.Schema.Schema) == 0 {
		return nil, errors.New("risk: Schema is required")
	}
	if strings.TrimSpace(cfg.Schema.Name) == "" {
		return nil, errors.New("risk: Schema.Name is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = shared.SystemClock{}
	}
	return &Analyzer{cfg: cfg}, nil
}

// Analyze classifies pr and returns an AnalysisResult of
// analysis.KindRisk that is ready to persist.
//
// Error handling:
//   - Transport / provider failures bubble up unchanged so callers can
//     match them against the llm.Err* sentinels via errors.Is.
//   - Malformed JSON or domain-invariant violations on the decoded
//     payload are wrapped with llm.ErrInvalidResponse — the response
//     came from the model but does not conform to the schema we asked
//     for, so it is treated as an invalid response.
func (a *Analyzer) Analyze(ctx context.Context, pr *pullrequest.PullRequest) (*analysis.AnalysisResult, error) {
	if pr == nil {
		return nil, errors.New("risk: nil pull request")
	}

	req := llm.Request{
		ModelRole: llm.RoleRisk,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: buildUserPrompt(pr)},
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
		return nil, fmt.Errorf("%w: risk payload: %v", llm.ErrInvalidResponse, err)
	}

	result, err := analysis.NewResult(
		analysis.KindRisk,
		analysis.SubjectForPR(pr.ID()),
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
		return nil, fmt.Errorf("risk: build analysis result: %w", err)
	}
	return result, nil
}
