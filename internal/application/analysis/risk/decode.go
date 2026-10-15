package risk

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/domain/analysis"
)

// schemaPayload mirrors the on-the-wire JSON shape declared by
// internal/infrastructure/openai/schemas/risk_report.json.
//
// We round-trip through this DTO rather than unmarshalling directly
// into analysis.RiskReport so the type conversions (int -> RiskLevel,
// string -> RiskFactor) stay explicit and the domain constructor —
// not json.Unmarshal — owns invariant enforcement.
type schemaPayload struct {
	Level       int      `json:"Level"`
	Factors     []string `json:"Factors"`
	Explanation string   `json:"Explanation"`
	Confidence  float64  `json:"Confidence"`
}

// decodePayload converts the model's JSON response into a validated
// domain RiskReport. Both JSON-shape and domain-invariant failures
// surface here and are wrapped with llm.ErrInvalidResponse by the
// caller in analyzer.go.
//
// As a small reliability hedge against models that ignore strict
// mode and wrap their JSON in a markdown code fence, we strip a
// leading/trailing ```...``` block before unmarshalling.
func decodePayload(raw []byte) (analysis.RiskReport, error) {
	raw = []byte(stripCodeFence(string(raw)))

	var p schemaPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return analysis.RiskReport{}, fmt.Errorf("unmarshal: %w", err)
	}

	factors := make([]analysis.RiskFactor, 0, len(p.Factors))
	for _, f := range p.Factors {
		factors = append(factors, analysis.RiskFactor(f))
	}

	return analysis.NewRiskReport(
		analysis.RiskLevel(p.Level),
		factors,
		p.Explanation,
		p.Confidence,
	)
}

// stripCodeFence removes a single surrounding markdown code fence if
// present. Handles plain ``` and language-tagged ```json. Idempotent
// and safe on non-fenced input (returns the trimmed string).
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
