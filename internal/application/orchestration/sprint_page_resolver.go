package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/sprint"
)

// ConfluenceCreator is the slice of the Confluence adapter the
// per-sprint page resolver needs. Kept narrow so tests can stub it
// without spinning up an HTTP server.
type ConfluenceCreator interface {
	CreatePage(ctx context.Context, in ConfluenceCreateInput) (ConfluenceCreatedPage, error)
}

// ConfluenceCreateInput mirrors the fields of
// infrastructure/confluence.CreateInput, redeclared locally so the
// application layer does not import the adapter package.
type ConfluenceCreateInput struct {
	SpaceKey    string
	ParentID    string
	Title       string
	BodyStorage string
}

// ConfluenceCreatedPage mirrors the tiny slice of *confluence.Page
// the resolver needs (only ID; URL is recomputed by the publisher on
// the subsequent update round-trip).
type ConfluenceCreatedPage struct {
	ID    string
	Title string
}

// SprintPageResolver returns the Confluence page id for a sprint,
// creating the page on first use and remembering the mapping in
// postgres so subsequent publishes hit the same page (idempotent).
//
// The resolver is only wired up in per-sprint mode; when nil, the
// release-notes handler falls back to a single static page id.
type SprintPageResolver struct {
	Client   ConfluenceCreator
	Store    persistence.SprintPages
	SpaceKey string
	ParentID string
	// HeadingPrefix (e.g. "Marvelous") is prepended to the sprint's
	// numeric suffix to form the page title -- e.g. "Marvelous
	// Sprint 187". Empty falls back to the raw sprint name.
	HeadingPrefix string
}

// NewSprintPageResolver validates deps and returns the resolver.
func NewSprintPageResolver(r SprintPageResolver) (*SprintPageResolver, error) {
	switch {
	case r.Client == nil:
		return nil, errors.New("orchestration: SprintPageResolver: Client is required")
	case r.Store == nil:
		return nil, errors.New("orchestration: SprintPageResolver: Store is required")
	case strings.TrimSpace(r.SpaceKey) == "":
		return nil, errors.New("orchestration: SprintPageResolver: SpaceKey is required")
	case strings.TrimSpace(r.ParentID) == "":
		return nil, errors.New("orchestration: SprintPageResolver: ParentID is required")
	}
	return &r, nil
}

// ResolveOrCreate returns the Confluence page id for the given sprint.
// If a mapping already exists in the sprint_confluence_page table it
// is returned; otherwise a new blank page is created under ParentID
// and the mapping is persisted before the id is returned.
//
// The created page is intentionally empty -- the release-notes
// publisher runs immediately after and populates it via its usual
// GetPage / splice / UpdatePage flow.
func (r *SprintPageResolver) ResolveOrCreate(ctx context.Context, sp *sprint.Sprint) (string, error) {
	if sp == nil {
		return "", errors.New("orchestration: SprintPageResolver: sprint is required")
	}
	sprintID := sp.ID()

	existing, err := r.Store.FindBySprintID(ctx, sprintID)
	switch {
	case err == nil:
		return existing.PageID, nil
	case !errors.Is(err, persistence.ErrNotFound):
		return "", fmt.Errorf("orchestration: SprintPageResolver: lookup mapping: %w", err)
	}

	title := r.pageTitle(sp)
	created, err := r.Client.CreatePage(ctx, ConfluenceCreateInput{
		SpaceKey: r.SpaceKey,
		ParentID: r.ParentID,
		Title:    title,
		// Empty body -- the publisher owns page content.
		BodyStorage: "",
	})
	if err != nil {
		return "", fmt.Errorf("orchestration: SprintPageResolver: create page: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("orchestration: SprintPageResolver: confluence returned empty page id")
	}

	if err := r.Store.Save(ctx, &persistence.SprintPageMapping{
		SprintID:  sprintID,
		PageID:    created.ID,
		PageTitle: title,
	}); err != nil {
		return "", fmt.Errorf("orchestration: SprintPageResolver: save mapping: %w", err)
	}
	return created.ID, nil
}

// pageTitle builds "<HeadingPrefix> Sprint <N>" using the numeric
// suffix already parsed by sprintNumberFromName. When the sprint has
// no numeric tail (e.g. "Ad-hoc"), we fall back to the raw name so
// the page still gets a sensible title.
func (r *SprintPageResolver) pageTitle(sp *sprint.Sprint) string {
	n := sprintNumberFromName(sp.Name())
	prefix := strings.TrimSpace(r.HeadingPrefix)
	if n == 0 {
		if prefix == "" {
			return sp.Name()
		}
		return prefix + " " + sp.Name()
	}
	if prefix == "" {
		return fmt.Sprintf("Sprint %d", n)
	}
	return fmt.Sprintf("%s Sprint %d", prefix, n)
}

// SprintPageIDResolver is the minimal interface the release-notes
// handler consumes. The concrete SprintPageResolver satisfies it;
// tests can stub with a func-typed adapter.
type SprintPageIDResolver interface {
	ResolveOrCreate(ctx context.Context, sp *sprint.Sprint) (string, error)
}

// Compile-time assertion.
var _ SprintPageIDResolver = (*SprintPageResolver)(nil)
