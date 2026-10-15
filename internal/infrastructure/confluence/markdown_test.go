package confluence

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkdownToStorage_Headings(t *testing.T) {
	got := MarkdownToStorage("# h1\n## h2\n### h3\n")
	assert.Equal(t, "<h1>h1</h1><h2>h2</h2><h3>h3</h3>", got)
}

func TestMarkdownToStorage_ParagraphCollapsesAdjacentLines(t *testing.T) {
	got := MarkdownToStorage("line one\nline two\n\nsecond para\n")
	assert.Equal(t, "<p>line one line two</p><p>second para</p>", got)
}

func TestMarkdownToStorage_UnorderedListGroupsConsecutiveItems(t *testing.T) {
	got := MarkdownToStorage("- one\n- two\n* three\n\n- four\n")
	// Trailing star-style item joins the same list (consecutive); blank line breaks it.
	assert.Equal(t, "<ul><li>one</li><li>two</li><li>three</li></ul><ul><li>four</li></ul>", got)
}

func TestMarkdownToStorage_InlineBoldCodeAndLink(t *testing.T) {
	got := MarkdownToStorage("this is **bold** and `code` and a [link](https://example.com/x).")
	assert.Equal(t,
		`<p>this is <strong>bold</strong> and <code>code</code> and a <a href="https://example.com/x">link</a>.</p>`,
		got)
}

func TestMarkdownToStorage_FencedCodeBlockPreservesContentVerbatimAndEscapes(t *testing.T) {
	got := MarkdownToStorage("```\nfunc f() { return a < b }\n```\n")
	assert.Equal(t, "<pre><code>func f() { return a &lt; b }\n</code></pre>", got)
}

func TestMarkdownToStorage_EscapesHTMLInLiteralText(t *testing.T) {
	got := MarkdownToStorage("a < b & c > d \"quoted\"")
	assert.Equal(t, `<p>a &lt; b &amp; c &gt; d &quot;quoted&quot;</p>`, got)
}

func TestMarkdownToStorage_ListItemEscapesHTML(t *testing.T) {
	got := MarkdownToStorage("- a<b>c</b>\n")
	assert.Equal(t, "<ul><li>a&lt;b&gt;c&lt;/b&gt;</li></ul>", got)
}

func TestMarkdownToStorage_EmptyInput(t *testing.T) {
	assert.Equal(t, "", MarkdownToStorage(""))
	assert.Equal(t, "", MarkdownToStorage("\n\n\n"))
}

func TestMarkdownToStorage_HeadingBreaksListAndParagraph(t *testing.T) {
	got := MarkdownToStorage("intro paragraph\n- item 1\n### Title\n- item A\n")
	// Paragraph closes before the list; list closes before the heading; new list opens after.
	assert.Equal(t,
		"<p>intro paragraph</p><ul><li>item 1</li></ul><h3>Title</h3><ul><li>item A</li></ul>",
		got)
}

func TestMarkdownToStorage_NormalizesCarriageReturns(t *testing.T) {
	got := MarkdownToStorage("line\r\n\r\nnext\r\n")
	assert.Equal(t, "<p>line</p><p>next</p>", got)
}

func TestMarkdownToStorage_LinkURLDoesNotAcceptSpaces(t *testing.T) {
	// reLink uses [^)\s] for the URL so a space-bearing pseudo-link is left alone.
	got := MarkdownToStorage("see [text](not a url)")
	require.NotContains(t, got, "<a ")
	// The literal brackets are HTML-escaped via the paragraph path
	// (no escape on '[' or '(' — those have no XHTML meaning).
	assert.Equal(t, "<p>see [text](not a url)</p>", got)
}

func TestRenderInline_BoldInsideListItem(t *testing.T) {
	// Bold transform happens regardless of block type because renderInline runs on every block.
	got := MarkdownToStorage("- ship **fast**\n")
	assert.True(t, strings.Contains(got, "<li>ship <strong>fast</strong></li>"))
}

func TestMarkdownToStorage_DropsUnsafeLinkSchemes(t *testing.T) {
	// javascript:, data:, vbscript:, file: must NEVER land as <a href="...">
	// in storage format \u2014 Confluence may render them and we would have
	// shipped a stored-XSS-shaped link.
	cases := []struct {
		name string
		md   string
		text string // link text that must survive as plain content
	}{
		{"javascript", "[click](javascript:alert%281%29)", "click"},
		{"data", "[click](data:text/html,<script>x</script>)", "click"},
		{"vbscript", "[click](vbscript:msgbox)", "click"},
		{"file", "[open](file:///etc/passwd)", "open"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := MarkdownToStorage(tc.md)
			assert.NotContains(t, got, "<a ", "unsafe scheme must not produce an anchor")
			assert.NotContains(t, got, "href=")
			// The link text survives as plain content (already HTML-escaped).
			assert.Contains(t, got, tc.text)
		})
	}
}

func TestMarkdownToStorage_AcceptsSafeLinkSchemes(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{"https", "[a](https://example.com/x)", `<a href="https://example.com/x">a</a>`},
		{"http", "[a](http://example.com/x)", `<a href="http://example.com/x">a</a>`},
		{"mailto", "[a](mailto:ops@example.com)", `<a href="mailto:ops@example.com">a</a>`},
		{"relative", "[a](/wiki/page)", `<a href="/wiki/page">a</a>`},
		{"anchor", "[a](#section)", `<a href="#section">a</a>`},
		{"mixed-case scheme", "[a](HTTPS://example.com)", `<a href="HTTPS://example.com">a</a>`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := MarkdownToStorage(tc.md)
			assert.Contains(t, got, tc.want)
		})
	}
}
