package qa

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/domain/analysis"
)

// schemaPayload mirrors the on-the-wire JSON shape declared by
// internal/infrastructure/openai/schemas/qa_recommendations.json.
//
// We round-trip through this DTO rather than unmarshalling directly
// into analysis.QARecommendations so the type conversions (int ->
// TestPriority) stay explicit and the domain constructor — not
// json.Unmarshal — owns invariant enforcement.
type schemaPayload struct {
	Summary         string           `json:"Summary"`
	RegressionAreas []string         `json:"RegressionAreas"`
	ImpactedModules []string         `json:"ImpactedModules"`
	Scenarios       []schemaScenario `json:"Scenarios"`
}

// schemaScenario mirrors the Scenarios[] item shape.
//
// Priority is the schema-side integer enum:
//
//	1 = P3 (nice to have)
//	2 = P2 (standard)
//	3 = P1 (must run before merge)
//	4 = P0 (blocker, sign-off required)
//
// This matches analysis.TestPriority's iota+1 ordering — see
// internal/domain/analysis/qa.go — so the cast in decodePayload is a
// no-op renumbering, not a translation table.
type schemaScenario struct {
	Title       string `json:"Title"`
	Description string `json:"Description"`
	Priority    int    `json:"Priority"`
	Module      string `json:"Module"`
}

// decodePayload converts the model's JSON response into a validated
// domain QARecommendations. Both JSON-shape and domain-invariant
// failures surface here and are wrapped with llm.ErrInvalidResponse by
// the caller in analyzer.go.
//
// As a small reliability hedge against models that ignore strict mode
// and wrap their JSON in a markdown code fence, we strip a
// leading/trailing ```...``` block before unmarshalling.
func decodePayload(raw []byte) (analysis.QARecommendations, error) {
	raw = []byte(stripCodeFence(string(raw)))

	var p schemaPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return analysis.QARecommendations{}, fmt.Errorf("unmarshal: %w", err)
	}

	scenarios := make([]analysis.TestScenario, 0, len(p.Scenarios))
	for _, s := range p.Scenarios {
		scenarios = append(scenarios, analysis.TestScenario{
			Title:       s.Title,
			Description: s.Description,
			Priority:    analysis.TestPriority(s.Priority),
			Module:      s.Module,
		})
	}

	return analysis.NewQARecommendations(
		p.Summary,
		p.RegressionAreas,
		p.ImpactedModules,
		scenarios,
	)
}

// stripCodeFence removes a single surrounding markdown code fence if
// present. Handles plain ``` and language-tagged ```json. Idempotent
// and safe on non-fenced input (returns the trimmed string).
//
// Duplicated from the risk analyzer rather than shared via a sibling
// package — extracting an internal helper package becomes worthwhile
// once a third analyzer needs the same hedge.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	// Skip the optional language tag up to the first newline.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, " \t\r\n")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
