package analysis

import (
	"strings"

	"github.com/repomind/repomind/internal/domain/shared"
)

// TestPriority orders QA test scenarios.
type TestPriority int

const (
	PriorityP3 TestPriority = iota + 1 // nice to have
	PriorityP2                         // standard
	PriorityP1                         // must run before merge
	PriorityP0                         // blocker — sign-off required
)

func (p TestPriority) String() string {
	switch p {
	case PriorityP0:
		return "P0"
	case PriorityP1:
		return "P1"
	case PriorityP2:
		return "P2"
	case PriorityP3:
		return "P3"
	}
	return "P?"
}

// TestScenario is a single recommended test case.
type TestScenario struct {
	Title       string
	Description string
	Priority    TestPriority
	Module      string // best-effort module/package name
}

// QARecommendations is the Payload produced by QARecommendationAgent.
type QARecommendations struct {
	RegressionAreas []string
	ImpactedModules []string
	Scenarios       []TestScenario
	Summary         string
}

// NewQARecommendations validates and constructs a QARecommendations.
func NewQARecommendations(
	summary string,
	regressionAreas []string,
	impactedModules []string,
	scenarios []TestScenario,
) (QARecommendations, error) {
	if strings.TrimSpace(summary) == "" {
		return QARecommendations{}, shared.NewValidationError("summary", "must not be empty")
	}
	if len(scenarios) == 0 {
		return QARecommendations{}, shared.NewValidationError("scenarios", "must contain at least one scenario")
	}
	for i, s := range scenarios {
		if strings.TrimSpace(s.Title) == "" {
			return QARecommendations{}, shared.NewValidationError("scenarios.title", "must not be empty at index "+itoa(i))
		}
		if s.Priority < PriorityP3 || s.Priority > PriorityP0 {
			return QARecommendations{}, shared.NewValidationError("scenarios.priority", "must be P0..P3 at index "+itoa(i))
		}
	}
	return QARecommendations{
		Summary:         strings.TrimSpace(summary),
		RegressionAreas: dedupeStrings(regressionAreas),
		ImpactedModules: dedupeStrings(impactedModules),
		Scenarios:       scenarios,
	}, nil
}

// payloadKind implements analysis.Payload.
func (QARecommendations) payloadKind() Kind { return KindQARecommendations }

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
