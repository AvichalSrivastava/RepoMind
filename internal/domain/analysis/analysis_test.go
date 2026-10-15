package analysis_test

import (
	"errors"
	"testing"
	"time"

	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRiskLevel(t *testing.T) {
	tests := map[string]analysis.RiskLevel{
		"low":      analysis.RiskLow,
		"Medium":   analysis.RiskMedium,
		"  HIGH ":  analysis.RiskHigh,
		"critical": analysis.RiskCritical,
	}
	for in, want := range tests {
		got, err := analysis.ParseRiskLevel(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}

	_, err := analysis.ParseRiskLevel("catastrophic")
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvalidArgument))
}

func TestRiskLevelOrdering(t *testing.T) {
	assert.True(t, analysis.RiskCritical.AtLeast(analysis.RiskHigh))
	assert.True(t, analysis.RiskHigh.AtLeast(analysis.RiskHigh))
	assert.False(t, analysis.RiskLow.AtLeast(analysis.RiskMedium))
}

func TestNewRiskReport_Validation(t *testing.T) {
	_, err := analysis.NewRiskReport(0, nil, "x", 0.5)
	require.Error(t, err)

	_, err = analysis.NewRiskReport(analysis.RiskHigh, nil, "", 0.5)
	require.Error(t, err)

	_, err = analysis.NewRiskReport(analysis.RiskHigh, nil, "ok", 1.5)
	require.Error(t, err)

	rep, err := analysis.NewRiskReport(
		analysis.RiskHigh,
		[]analysis.RiskFactor{analysis.FactorDatabaseMigration, analysis.FactorDatabaseMigration, analysis.FactorAuthenticationChange},
		"Migration touches users table and login handler.",
		0.85,
	)
	require.NoError(t, err)
	assert.Len(t, rep.Factors, 2, "factors deduplicated")
	assert.Equal(t, analysis.RiskHigh, rep.Level)
}

func TestNewResult_EnforcesPayloadKindAgreement(t *testing.T) {
	rep, err := analysis.NewRiskReport(analysis.RiskMedium, nil, "ok", 0.7)
	require.NoError(t, err)

	prID := shared.NewPullRequestID()
	clk := shared.FixedClock{T: time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)}

	// Happy path.
	res, err := analysis.NewResult(
		analysis.KindRisk,
		analysis.SubjectForPR(prID),
		analysis.AnalyzerRef{Name: "RiskAnalyzer", Version: "1.0.0", PromptVersion: "1"},
		rep,
		"gpt-4o-mini",
		1234,
		clk,
	)
	require.NoError(t, err)
	assert.Equal(t, analysis.KindRisk, res.Kind())
	assert.Equal(t, clk.Now(), res.GeneratedAt())

	// Mismatched kind / payload must fail.
	_, err = analysis.NewResult(
		analysis.KindQARecommendations,
		analysis.SubjectForPR(prID),
		analysis.AnalyzerRef{Name: "x"},
		rep,
		"gpt-4o-mini",
		0,
		clk,
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvariant))
}
