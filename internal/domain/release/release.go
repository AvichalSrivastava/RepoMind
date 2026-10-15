// Package release contains the Release aggregate — a versioned bundle
// of merged PullRequests delivered as a single deployable unit, with
// accompanying release notes.
package release

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/shared"
)

// Status models the release lifecycle.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
)

// Version is a validated SemVer string (vMAJOR.MINOR.PATCH with optional pre-release).
type Version struct {
	value string
}

var semverPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.\-]+)?(?:\+[0-9A-Za-z.\-]+)?$`)

// NewVersion validates and constructs a Version.
func NewVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if !semverPattern.MatchString(s) {
		return Version{}, shared.NewValidationError("version", fmt.Sprintf("invalid SemVer %q", s))
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	return Version{value: s}, nil
}

func (v Version) String() string { return v.value }
func (v Version) IsZero() bool   { return v.value == "" }

// Release is the aggregate root.
//
// Invariants:
//   - Version is non-zero.
//   - Sprint reference is set.
//   - PRs are deduplicated by ID.
//   - Notes can only be attached when at least one PR is present.
//   - Once Published, the release is immutable.
//   - ContentHash, when set, is an opaque fingerprint of the inputs
//     that produced this release (sprint + PRs). Used by the M10
//     release-notes use case to skip republishing identical content.
//     Empty string means "unknown" (legacy rows from before the
//     content_hash column existed).
type Release struct {
	id          shared.ReleaseID
	sprintID    shared.SprintID
	version     Version
	status      Status
	pullReqs    []shared.PullRequestID
	notes       string // markdown body
	contentHash string
	createdAt   time.Time
	publishedAt *time.Time

	emitter shared.EventEmitter
}

// DraftInput holds parameters for drafting a release.
type DraftInput struct {
	SprintID shared.SprintID
	Version  Version
	// ContentHash is the opaque fingerprint of the inputs that
	// produced this release. The M10 release-notes use case sets
	// it from sha256(sprint_id + sorted PR ids + merge SHAs).
	// Empty is allowed but inhibits L3 dedup short-circuits on
	// future runs.
	ContentHash string
}

// Draft creates a new Release in Draft status.
func Draft(in DraftInput, clock shared.Clock) (*Release, error) {
	if in.SprintID.IsZero() {
		return nil, shared.NewValidationError("sprint_id", "must not be zero")
	}
	if in.Version.IsZero() {
		return nil, shared.NewValidationError("version", "must be provided")
	}
	now := clock.Now()
	r := &Release{
		id:          shared.NewReleaseID(),
		sprintID:    in.SprintID,
		version:     in.Version,
		status:      StatusDraft,
		contentHash: in.ContentHash,
		createdAt:   now,
	}
	r.emitter.Record(DraftedEvent{
		BaseEvent: shared.NewBaseEvent(r.id.String(), now),
		Version:   in.Version.String(),
	})
	return r, nil
}

// Rehydrate reconstructs a Release from persisted state.
func Rehydrate(
	id shared.ReleaseID,
	sprintID shared.SprintID,
	version Version,
	status Status,
	prIDs []shared.PullRequestID,
	notes string,
	contentHash string,
	createdAt time.Time,
	publishedAt *time.Time,
) *Release {
	return &Release{
		id:          id,
		sprintID:    sprintID,
		version:     version,
		status:      status,
		pullReqs:    append([]shared.PullRequestID(nil), prIDs...),
		notes:       notes,
		contentHash: contentHash,
		createdAt:   createdAt,
		publishedAt: publishedAt,
	}
}

// ---------- Accessors ----------

func (r *Release) ID() shared.ReleaseID      { return r.id }
func (r *Release) SprintID() shared.SprintID { return r.sprintID }
func (r *Release) Version() Version          { return r.version }
func (r *Release) Status() Status            { return r.status }
func (r *Release) Notes() string             { return r.notes }
func (r *Release) ContentHash() string       { return r.contentHash }
func (r *Release) PullRequests() []shared.PullRequestID {
	return append([]shared.PullRequestID(nil), r.pullReqs...)
}
func (r *Release) CreatedAt() time.Time             { return r.createdAt }
func (r *Release) PublishedAt() *time.Time          { return r.publishedAt }
func (r *Release) PullEvents() []shared.DomainEvent { return r.emitter.PullEvents() }
func (r *Release) IsPublished() bool                { return r.status == StatusPublished }

// ---------- Behavior ----------

// IncludePullRequest attaches a merged PR to the draft release.
// Duplicates are ignored. Rejects mutations after publishing.
func (r *Release) IncludePullRequest(id shared.PullRequestID) error {
	if r.IsPublished() {
		return fmt.Errorf("%w: cannot modify a published release", shared.ErrInvariant)
	}
	if id.IsZero() {
		return shared.NewValidationError("pull_request_id", "must not be zero")
	}
	for _, existing := range r.pullReqs {
		if existing.String() == id.String() {
			return nil
		}
	}
	r.pullReqs = append(r.pullReqs, id)
	return nil
}

// AttachNotes sets the markdown release-notes body.
func (r *Release) AttachNotes(markdown string) error {
	if r.IsPublished() {
		return fmt.Errorf("%w: cannot modify a published release", shared.ErrInvariant)
	}
	if strings.TrimSpace(markdown) == "" {
		return shared.NewValidationError("notes", "must not be empty")
	}
	if len(r.pullReqs) == 0 {
		return fmt.Errorf("%w: release has no PRs", shared.ErrInvariant)
	}
	r.notes = markdown
	return nil
}

// Publish freezes the release. Idempotent.
func (r *Release) Publish(at time.Time) error {
	if r.IsPublished() {
		return nil
	}
	if len(r.pullReqs) == 0 {
		return fmt.Errorf("%w: cannot publish a release with no PRs", shared.ErrInvariant)
	}
	if strings.TrimSpace(r.notes) == "" {
		return fmt.Errorf("%w: cannot publish a release without notes", shared.ErrInvariant)
	}
	r.status = StatusPublished
	r.publishedAt = &at
	r.emitter.Record(PublishedEvent{
		BaseEvent: shared.NewBaseEvent(r.id.String(), at),
		Version:   r.version.String(),
	})
	return nil
}

// ---------- Events ----------

type DraftedEvent struct {
	shared.BaseEvent
	Version string
}

func (DraftedEvent) EventName() string { return "release.drafted" }

type PublishedEvent struct {
	shared.BaseEvent
	Version string
}

func (PublishedEvent) EventName() string { return "release.published" }
