package analysis

import (
	"encoding/json"
	"fmt"
)

// MarshalPayload serializes a Payload to JSON. The wire form is just
// the struct's exported fields — kind is carried separately in the
// envelope (typically a DB column or message header).
//
// Returns an error only when the payload itself is unmarshalable
// (which would be a programmer bug — all current payload types are
// plain structs).
func MarshalPayload(p Payload) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("analysis: nil payload")
	}
	return json.Marshal(p)
}

// UnmarshalPayload reconstructs a Payload from JSON bytes given its
// kind. Persistence adapters call this when rehydrating
// AnalysisResult from storage.
//
// Adding a new Kind requires:
//  1. Adding the Kind constant and updating Kind.Valid().
//  2. Implementing a Payload type whose payloadKind() returns the new Kind.
//  3. Adding a case here.
func UnmarshalPayload(kind Kind, data []byte) (Payload, error) {
	switch kind {
	case KindRisk:
		var p RiskReport
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("analysis: unmarshal risk payload: %w", err)
		}
		return p, nil
	case KindReleaseNotes:
		var p ReleaseNotes
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("analysis: unmarshal release_notes payload: %w", err)
		}
		return p, nil
	case KindQARecommendations:
		var p QARecommendations
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("analysis: unmarshal qa_recommendations payload: %w", err)
		}
		return p, nil
	}
	return nil, fmt.Errorf("analysis: unknown kind %q", kind)
}
