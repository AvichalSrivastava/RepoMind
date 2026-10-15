// Package shared contains the domain shared kernel: types reused across
// every aggregate (IDs, errors, events, clock, primitive value objects).
//
// This package — and the whole internal/domain tree — must never import
// anything outside the standard library and a small allow-list of
// primitive helpers (currently: github.com/google/uuid).
package shared

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ---------- ID base ----------

// rawID is the underlying primitive for every typed identifier. We keep
// it unexported so aggregates can only construct their own ID type.
type rawID struct {
	v uuid.UUID
}

func newRawID() rawID           { return rawID{v: uuid.New()} }
func (r rawID) String() string  { return r.v.String() }
func (r rawID) IsZero() bool    { return r.v == uuid.Nil }
func (r rawID) UUID() uuid.UUID { return r.v }
func parseRaw(s string) (rawID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return rawID{}, fmt.Errorf("invalid id %q: %w", s, err)
	}
	return rawID{v: u}, nil
}

// ---------- Concrete IDs ----------
//
// Each aggregate gets a *distinct* named type wrapping rawID. Because
// they are different types, the Go compiler prevents passing a
// PullRequestID where a RepositoryID is expected.

// RepositoryID identifies a tracked Repository aggregate.
type RepositoryID struct{ rawID }

// NewRepositoryID returns a fresh random RepositoryID.
func NewRepositoryID() RepositoryID { return RepositoryID{newRawID()} }

// ParseRepositoryID parses a string into a RepositoryID.
func ParseRepositoryID(s string) (RepositoryID, error) {
	r, err := parseRaw(s)
	return RepositoryID{r}, err
}

// PullRequestID identifies a PullRequest aggregate.
type PullRequestID struct{ rawID }

func NewPullRequestID() PullRequestID { return PullRequestID{newRawID()} }
func ParsePullRequestID(s string) (PullRequestID, error) {
	r, err := parseRaw(s)
	return PullRequestID{r}, err
}

// SprintID identifies a Sprint aggregate.
type SprintID struct{ rawID }

func NewSprintID() SprintID { return SprintID{newRawID()} }
func ParseSprintID(s string) (SprintID, error) {
	r, err := parseRaw(s)
	return SprintID{r}, err
}

// ReleaseID identifies a Release aggregate.
type ReleaseID struct{ rawID }

func NewReleaseID() ReleaseID { return ReleaseID{newRawID()} }
func ParseReleaseID(s string) (ReleaseID, error) {
	r, err := parseRaw(s)
	return ReleaseID{r}, err
}

// AnalysisID identifies an AnalysisResult aggregate.
type AnalysisID struct{ rawID }

func NewAnalysisID() AnalysisID { return AnalysisID{newRawID()} }
func ParseAnalysisID(s string) (AnalysisID, error) {
	r, err := parseRaw(s)
	return AnalysisID{r}, err
}

// ---------- ID-related errors ----------

// ErrInvalidID is returned when a supplied id cannot be parsed.
var ErrInvalidID = errors.New("invalid identifier")
