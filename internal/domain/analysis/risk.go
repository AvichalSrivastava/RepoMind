package analysis

import (
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/domain/shared"
)

// RiskLevel is an ordered enum of risk severities. Comparison is
// total: Critical > High > Medium > Low.
type RiskLevel int

const (
	RiskLow RiskLevel = iota + 1
	RiskMedium
	RiskHigh
	RiskCritical
)

// ParseRiskLevel converts a case-insensitive string into a RiskLevel.
func ParseRiskLevel(s string) (RiskLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return RiskLow, nil
	case "medium":
		return RiskMedium, nil
	case "high":
		return RiskHigh, nil
	case "critical":
		return RiskCritical, nil
	}
	return 0, shared.NewValidationError("risk_level", fmt.Sprintf("unknown risk level %q", s))
}

// String returns the lower-case canonical name.
func (r RiskLevel) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	case RiskCritical:
		return "critical"
	}
	return "unknown"
}

// AtLeast returns true if r is at least as severe as other.
func (r RiskLevel) AtLeast(other RiskLevel) bool { return r >= other }

// RiskFactor names one observed signal contributing to the risk score.
type RiskFactor string

const (
	FactorManyFiles            RiskFactor = "many_files"
	FactorLargeDiff            RiskFactor = "large_diff"
	FactorDatabaseMigration    RiskFactor = "database_migration"
	FactorAuthenticationChange RiskFactor = "authentication_change"
	FactorPaymentChange        RiskFactor = "payment_change"
	FactorInfraConfigChange    RiskFactor = "infra_config_change"
	FactorPublicAPIChange      RiskFactor = "public_api_change"
	FactorMinimalReview        RiskFactor = "minimal_review"
)

// RiskReport is the Payload produced by RiskAnalysisAgent.
type RiskReport struct {
	Level       RiskLevel
	Factors     []RiskFactor
	Explanation string  // markdown — AI-generated rationale
	Confidence  float64 // 0..1
}

// NewRiskReport validates and constructs a RiskReport.
func NewRiskReport(level RiskLevel, factors []RiskFactor, explanation string, confidence float64) (RiskReport, error) {
	if level < RiskLow || level > RiskCritical {
		return RiskReport{}, shared.NewValidationError("risk_level", "must be Low|Medium|High|Critical")
	}
	if strings.TrimSpace(explanation) == "" {
		return RiskReport{}, shared.NewValidationError("explanation", "must not be empty")
	}
	if confidence < 0 || confidence > 1 {
		return RiskReport{}, shared.NewValidationError("confidence", "must be within [0, 1]")
	}
	return RiskReport{
		Level:       level,
		Factors:     dedupeFactors(factors),
		Explanation: strings.TrimSpace(explanation),
		Confidence:  confidence,
	}, nil
}

// payloadKind implements analysis.Payload.
func (RiskReport) payloadKind() Kind { return KindRisk }

func dedupeFactors(in []RiskFactor) []RiskFactor {
	seen := make(map[RiskFactor]struct{}, len(in))
	out := make([]RiskFactor, 0, len(in))
	for _, f := range in {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}
