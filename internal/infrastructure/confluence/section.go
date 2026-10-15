package confluence

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Section markers are HTML comments that Confluence preserves
// verbatim in the storage format. They let us round-trip an opaque
// region of a page without parsing the surrounding XHTML — which
// would be a fool's errand, since Confluence emits a dialect with
// many extensions (`<ac:structured-macro>`, `<ri:user>`, etc.).
//
// Format (whitespace-tolerant, name attribute is required):
//
//	<!-- repomind:section:start name="release-notes-MARV-42" -->
//	  ...arbitrary XHTML rendered by the publisher...
//	<!-- repomind:section:end name="release-notes-MARV-42" -->
//
// The publisher locates the matching start/end pair by name and
// replaces everything between them. If no marker is present we APPEND
// a fresh section (so first-publish is a write, not an error).
//
// Multiple markers with the same name are rejected to prevent silent
// page corruption (we would otherwise clobber the wrong region).

// errSectionInconsistent is returned when the page body contains
// duplicate or unbalanced markers — situations the publisher must
// surface as publish.ErrInvalidPage rather than guess at.
var errSectionInconsistent = errors.New("confluence: section markers are duplicate or unbalanced")

// startMarker / endMarker render the canonical form (used when
// appending a new section).
func startMarker(name string) string {
	return fmt.Sprintf(`<!-- repomind:section:start name=%q -->`, name)
}

func endMarker(name string) string {
	return fmt.Sprintf(`<!-- repomind:section:end name=%q -->`, name)
}

// nameRe accepts only safe section names: ASCII alphanumerics, dash,
// underscore, period. This bounds what callers can splice into the
// marker comment and avoids regex-injection at lookup time.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// replaceSection returns body with the named section replaced by
// newContent. If the marker pair is absent, a fresh section
// (markers + newContent) is appended after body, separated by a
// blank line. Inputs are not mutated.
//
// Returns errSectionInconsistent when more than one start (or more
// than one end) marker is present for the requested name, or when
// only one half of the pair is present.
func replaceSection(body, name, newContent string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("confluence: invalid section name %q", name)
	}

	startRe := regexp.MustCompile(`<!--\s*repomind:section:start\s+name="` + regexp.QuoteMeta(name) + `"\s*-->`)
	endRe := regexp.MustCompile(`<!--\s*repomind:section:end\s+name="` + regexp.QuoteMeta(name) + `"\s*-->`)

	starts := startRe.FindAllStringIndex(body, -1)
	ends := endRe.FindAllStringIndex(body, -1)

	switch {
	case len(starts) == 0 && len(ends) == 0:
		// First publish — append a fresh block.
		var b strings.Builder
		b.WriteString(strings.TrimRight(body, "\n"))
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(startMarker(name))
		b.WriteByte('\n')
		b.WriteString(newContent)
		b.WriteByte('\n')
		b.WriteString(endMarker(name))
		return b.String(), nil

	case len(starts) != 1 || len(ends) != 1:
		return "", errSectionInconsistent

	case starts[0][1] > ends[0][0]:
		// start appears after end → unbalanced.
		return "", errSectionInconsistent
	}

	// Splice: keep prefix through the start marker, the new content,
	// then the end marker through suffix.
	startEnd := starts[0][1]
	endStart := ends[0][0]
	var b strings.Builder
	b.WriteString(body[:startEnd])
	b.WriteByte('\n')
	b.WriteString(newContent)
	b.WriteByte('\n')
	b.WriteString(body[endStart:])
	return b.String(), nil
}
