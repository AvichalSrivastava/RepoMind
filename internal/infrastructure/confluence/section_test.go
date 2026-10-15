package confluence

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplaceSection_AppendsWhenMarkersAbsent(t *testing.T) {
	body := "<p>existing page content</p>"
	got, err := replaceSection(body, "release-notes-MARV-42", "<h2>new</h2>")
	require.NoError(t, err)

	assert.Contains(t, got, "<p>existing page content</p>")
	assert.Contains(t, got, `<!-- repomind:section:start name="release-notes-MARV-42" -->`)
	assert.Contains(t, got, "<h2>new</h2>")
	assert.Contains(t, got, `<!-- repomind:section:end name="release-notes-MARV-42" -->`)
	// New marker pair comes after the existing content.
	assert.True(t, strings.Index(got, "existing page content") <
		strings.Index(got, "repomind:section:start"))
}

func TestReplaceSection_ReplacesContentBetweenExistingMarkers(t *testing.T) {
	body := `<p>preamble</p>` +
		`<!-- repomind:section:start name="rn-1" -->` +
		`<h2>OLD</h2>` +
		`<!-- repomind:section:end name="rn-1" -->` +
		`<p>postscript</p>`

	got, err := replaceSection(body, "rn-1", "<h2>NEW</h2>")
	require.NoError(t, err)

	assert.Contains(t, got, "<p>preamble</p>")
	assert.Contains(t, got, "<p>postscript</p>")
	assert.Contains(t, got, "<h2>NEW</h2>")
	assert.NotContains(t, got, "<h2>OLD</h2>", "old section body must be evicted")
	// Markers themselves are preserved.
	assert.Contains(t, got, `repomind:section:start name="rn-1"`)
	assert.Contains(t, got, `repomind:section:end name="rn-1"`)
}

func TestReplaceSection_DoesNotTouchOtherSections(t *testing.T) {
	body := `<!-- repomind:section:start name="a" --><h2>A</h2><!-- repomind:section:end name="a" -->` +
		`<!-- repomind:section:start name="b" --><h2>B</h2><!-- repomind:section:end name="b" -->`

	got, err := replaceSection(body, "b", "<h2>B2</h2>")
	require.NoError(t, err)

	assert.Contains(t, got, "<h2>A</h2>")
	assert.Contains(t, got, "<h2>B2</h2>")
	assert.NotContains(t, got, "<h2>B</h2>")
}

func TestReplaceSection_RejectsDuplicateStartMarkers(t *testing.T) {
	body := `<!-- repomind:section:start name="x" -->one<!-- repomind:section:end name="x" -->` +
		`<!-- repomind:section:start name="x" -->two<!-- repomind:section:end name="x" -->`
	_, err := replaceSection(body, "x", "Z")
	require.ErrorIs(t, err, errSectionInconsistent)
}

func TestReplaceSection_RejectsUnbalancedMarkers(t *testing.T) {
	t.Run("start without end", func(t *testing.T) {
		body := `<!-- repomind:section:start name="x" -->dangling`
		_, err := replaceSection(body, "x", "Z")
		require.ErrorIs(t, err, errSectionInconsistent)
	})
	t.Run("end without start", func(t *testing.T) {
		body := `dangling<!-- repomind:section:end name="x" -->`
		_, err := replaceSection(body, "x", "Z")
		require.ErrorIs(t, err, errSectionInconsistent)
	})
	t.Run("start appears after end", func(t *testing.T) {
		body := `<!-- repomind:section:end name="x" -->...<!-- repomind:section:start name="x" -->`
		_, err := replaceSection(body, "x", "Z")
		require.ErrorIs(t, err, errSectionInconsistent)
	})
}

func TestReplaceSection_RejectsInvalidName(t *testing.T) {
	cases := []string{
		"",                // empty
		"has space",       // space disallowed
		`name"with"quote`, // quotes would break the marker
		"name<inject>",    // would inject HTML
		"name/slash",      // slash disallowed
	}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			_, err := replaceSection("body", name, "Z")
			require.Error(t, err)
		})
	}
}

func TestReplaceSection_NameOnlyMatchesExactSection(t *testing.T) {
	// A section named "foo" must NOT match a section named "foo-bar".
	body := `<!-- repomind:section:start name="foo-bar" --><h2>kept</h2><!-- repomind:section:end name="foo-bar" -->`
	got, err := replaceSection(body, "foo", "<h2>new</h2>")
	require.NoError(t, err)

	// Original "foo-bar" block is untouched...
	assert.Contains(t, got, `<!-- repomind:section:start name="foo-bar" -->`)
	assert.Contains(t, got, "<h2>kept</h2>")
	// ...and a fresh "foo" block is appended.
	assert.Contains(t, got, `<!-- repomind:section:start name="foo" -->`)
	assert.Contains(t, got, "<h2>new</h2>")
}

func TestReplaceSection_ToleratesExtraWhitespaceInMarkers(t *testing.T) {
	body := "<!--   repomind:section:start   name=\"x\"   -->OLD<!--  repomind:section:end  name=\"x\"  -->"
	got, err := replaceSection(body, "x", "NEW")
	require.NoError(t, err)
	assert.Contains(t, got, "NEW")
	assert.NotContains(t, got, "OLD")
}
