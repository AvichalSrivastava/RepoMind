package confluence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/application/publish"
)

// Compile-time guarantee that *Publisher satisfies the application port.
var _ publish.Publisher = (*Publisher)(nil)

// Publisher adapts a *Client to the publish.Publisher port: it
// renders markdown to storage format, splices the rendered block
// into a named section on the target page, and writes the new
// version — with bounded retry on 409 version conflicts (re-fetching
// and re-splicing against the latest body each time).
type Publisher struct {
	client     *Client
	maxRetries int
}

// NewPublisher wraps a Client. The Client's own MaxRetries handles
// network transients (429 / 5xx); the publisher's MaxRetries handles
// application-level 409 conflicts which require a fresh GET-then-PUT.
func NewPublisher(client *Client) *Publisher {
	if client == nil {
		return nil
	}
	return &Publisher{
		client:     client,
		maxRetries: client.cfg.MaxRetries,
	}
}

// Publish implements publish.Publisher.
//
// Steps:
//  1. Validate input.
//  2. GET the target page (current version + storage body).
//  3. Render the release notes to a storage-format block.
//  4. Splice the block into the named section (replace existing, or
//     append a fresh marker pair on first publish).
//  5. PUT with version = current + 1.
//  6. On 409, restart from step 2 up to maxRetries times — the body
//     may have been edited by another agent between our GET and PUT.
func (p *Publisher) Publish(ctx context.Context, in publish.Input) (*publish.Result, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}

	hasNarrative := in.Notes != nil
	var rendered string
	if hasNarrative {
		rendered = renderSection(in)
	}

	attempts := p.maxRetries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		page, err := p.client.GetPage(ctx, in.PageID)
		if err != nil {
			return nil, translateErr(err)
		}

		newBody := page.BodyStorage

		if hasNarrative {
			newBody, err = replaceSection(newBody, in.SectionName, rendered)
			if err != nil {
				if errors.Is(err, errSectionInconsistent) {
					return nil, fmt.Errorf("%w: section %q markers are duplicate or unbalanced", publish.ErrInvalidPage, in.SectionName)
				}
				return nil, err
			}
		}

		if len(in.LogRows) > 0 {
			newBody, _, err = upsertReleaseLogRows(newBody, in.LogRows, in.ForceRefresh)
			if err != nil {
				if errors.Is(err, errSectionInconsistent) {
					return nil, fmt.Errorf("%w: release-log-table markers are duplicate or unbalanced", publish.ErrInvalidPage)
				}
				return nil, err
			}
		}

		// Unlinked-PRs section: authoritative each publish. When the
		// caller passes a non-empty slice we always replace the section
		// body. When empty, we only touch the page if a stale section
		// already exists (in which case we clear it), so first
		// publishes with zero orphans don't leave an empty placeholder
		// behind.
		if len(in.UnlinkedPRs) > 0 {
			rendered := renderUnlinkedPRsSection(in.UnlinkedPRs)
			newBody, err = replaceSection(newBody, unlinkedPRsSectionName, rendered)
			if err != nil {
				if errors.Is(err, errSectionInconsistent) {
					return nil, fmt.Errorf("%w: unlinked-prs-table markers are duplicate or unbalanced", publish.ErrInvalidPage)
				}
				return nil, err
			}
		} else if hasUnlinkedSection(newBody) {
			newBody, err = replaceSection(newBody, unlinkedPRsSectionName, "")
			if err != nil {
				if errors.Is(err, errSectionInconsistent) {
					return nil, fmt.Errorf("%w: unlinked-prs-table markers are duplicate or unbalanced", publish.ErrInvalidPage)
				}
				return nil, err
			}
		}

		updated, err := p.client.UpdatePage(ctx, UpdateInput{
			ID:          page.ID,
			Title:       page.Title,
			Type:        page.Type,
			NewVersion:  page.Version + 1,
			BodyStorage: newBody,
		})
		if err == nil {
			return &publish.Result{
				PageID:     updated.ID,
				NewVersion: updated.Version,
				URL:        updated.WebURL,
			}, nil
		}

		if errors.Is(err, ErrConflict) {
			lastErr = translateErr(err)
			continue // re-fetch and re-splice
		}
		return nil, translateErr(err)
	}
	return nil, lastErr
}

// validateInput rejects calls that are obviously malformed at the
// adapter boundary. Domain-shape rules (non-empty Summary, Markdown,
// Entries) are guaranteed by analysis.NewReleaseNotes upstream, so we
// only re-check the parts the caller assembles here.
func validateInput(in publish.Input) error {
	if strings.TrimSpace(in.PageID) == "" {
		return errors.New("confluence: input.PageID is required")
	}
	if in.Notes == nil && len(in.LogRows) == 0 && len(in.UnlinkedPRs) == 0 {
		return errors.New("confluence: input must carry at least Notes, LogRows, or UnlinkedPRs")
	}
	if in.Notes != nil {
		if !nameRe.MatchString(in.SectionName) {
			return fmt.Errorf("confluence: invalid input.SectionName %q (allowed: %s)", in.SectionName, nameRe.String())
		}
		if strings.TrimSpace(in.Heading) == "" {
			return errors.New("confluence: input.Heading is required when Notes is set")
		}
	}
	return nil
}

// renderSection produces the XHTML body that lives BETWEEN the
// section markers. The layout is:
//
//	<h2>Heading</h2>
//	<p><em>Generated by RepoMind at {timestamp}.</em></p>
//	<p>{Summary}</p>
//	{Markdown rendered to storage XHTML}
//
// We deliberately render Summary as its own paragraph above the
// markdown body even though the markdown may also include the
// summary — operators have asked for an at-a-glance line at the top
// of every section without needing to read the model's prose.
func renderSection(in publish.Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<h2>%s</h2>", escapeHTML(in.Heading))
	if !in.GeneratedAt.IsZero() {
		fmt.Fprintf(&b, "<p><em>Generated by RepoMind at %s.</em></p>",
			escapeHTML(in.GeneratedAt.UTC().Format(time.RFC3339)))
	}
	if s := strings.TrimSpace(in.Notes.Summary); s != "" {
		fmt.Fprintf(&b, "<p>%s</p>", escapeHTML(s))
	}
	b.WriteString(MarkdownToStorage(in.Notes.Markdown))
	return b.String()
}

// translateErr maps confluence-package sentinels onto the
// vendor-neutral publish-package sentinels so callers in the
// application layer never need to import this package to discriminate
// failures. Unknown errors pass through unwrapped.
func translateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnauthorized):
		return fmt.Errorf("%w: %v", publish.ErrUnauthorized, err)
	case errors.Is(err, ErrForbidden):
		return fmt.Errorf("%w: %v", publish.ErrForbidden, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", publish.ErrNotFound, err)
	case errors.Is(err, ErrRateLimited):
		return fmt.Errorf("%w: %v", publish.ErrRateLimited, err)
	case errors.Is(err, ErrServer):
		return fmt.Errorf("%w: %v", publish.ErrServer, err)
	case errors.Is(err, ErrConflict):
		return fmt.Errorf("%w: %v", publish.ErrConflict, err)
	default:
		return err
	}
}
