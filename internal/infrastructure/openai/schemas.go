package openai

import (
	_ "embed"

	"github.com/repomind/repomind/internal/application/llm"
	"github.com/repomind/repomind/internal/domain/analysis"
)

// Schemas for the three polymorphic analysis payloads in the domain
// layer. They are exposed as llm.StructuredOutputSpec values so
// analyzer code can hand them to llm.Request without rebuilding the
// JSON each call.
//
// The schemas live with the adapter (not the domain) because they are
// a wire-format concern. The domain types (analysis.RiskReport etc.)
// remain plain Go structs.

//go:embed schemas/risk_report.json
var riskReportSchema []byte

//go:embed schemas/release_notes.json
var releaseNotesSchema []byte

//go:embed schemas/qa_recommendations.json
var qaRecommendationsSchema []byte

// SchemaFor returns the StructuredOutputSpec for one of the domain
// analysis kinds. The Strict flag is set to true so the provider
// rejects non-conforming output; adapters that don't support strict
// mode downgrade silently inside the adapter.
func SchemaFor(kind analysis.Kind) (*llm.StructuredOutputSpec, bool) {
	switch kind {
	case analysis.KindRisk:
		return &llm.StructuredOutputSpec{
			Name:   "risk_report",
			Schema: riskReportSchema,
			Strict: true,
		}, true
	case analysis.KindReleaseNotes:
		return &llm.StructuredOutputSpec{
			Name:   "release_notes",
			Schema: releaseNotesSchema,
			Strict: true,
		}, true
	case analysis.KindQARecommendations:
		return &llm.StructuredOutputSpec{
			Name:   "qa_recommendations",
			Schema: qaRecommendationsSchema,
			Strict: true,
		}, true
	}
	return nil, false
}
