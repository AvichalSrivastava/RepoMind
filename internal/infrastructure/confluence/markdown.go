package confluence

import (
	"fmt"
	"regexp"
	"strings"
)

// MarkdownToStorage converts a deliberately small subset of Markdown
// into the Confluence storage format (XHTML).
//
// The subset is exactly what the M7 release-notes prompt is
// instructed to emit (see internal/application/analysis/releasenotes/prompt.go):
//
//   - H1 / H2 / H3 ATX headings (`# `, `## `, `### `)
//   - Unordered list items (`- ` or `* `) — runs of consecutive items
//     are grouped into a single <ul>
//   - Blank-line-separated paragraphs
//   - Inline `**bold**`, “ `code` “, and `[text](url)` links
//   - Triple-backtick fenced code blocks → <pre><code>...</code></pre>
//
// Anything else (tables, blockquotes, images, ordered lists) renders
// as plain text inside a <p>. The model is instructed not to produce
// them; the converter is intentionally narrow rather than trying to
// be a full CommonMark engine — that's a strictly worse fight to
// pick against Confluence's quirks.
//
// All emitted markup HTML-escapes literal text before applying inline
// transforms so user / model content cannot inject raw XHTML into the
// page.
func MarkdownToStorage(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	lines := strings.Split(md, "\n")

	var (
		out         strings.Builder
		inList      bool
		inCodeBlock bool
		codeBuf     strings.Builder
		paraBuf     []string
	)

	flushPara := func() {
		if len(paraBuf) == 0 {
			return
		}
		joined := strings.Join(paraBuf, " ")
		out.WriteString("<p>")
		out.WriteString(renderInline(joined))
		out.WriteString("</p>")
		paraBuf = nil
	}
	flushList := func() {
		if inList {
			out.WriteString("</ul>")
			inList = false
		}
	}
	flushCode := func() {
		out.WriteString("<pre><code>")
		out.WriteString(escapeHTML(codeBuf.String()))
		out.WriteString("</code></pre>")
		codeBuf.Reset()
		inCodeBlock = false
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")

		// Inside a fenced code block, accumulate verbatim until close.
		if inCodeBlock {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				flushCode()
				continue
			}
			codeBuf.WriteString(raw)
			codeBuf.WriteByte('\n')
			continue
		}

		trimmed := strings.TrimSpace(line)

		// Open fenced code block.
		if strings.HasPrefix(trimmed, "```") {
			flushPara()
			flushList()
			inCodeBlock = true
			continue
		}

		// Blank line → paragraph / list break.
		if trimmed == "" {
			flushPara()
			flushList()
			continue
		}

		// Headings (H1/H2/H3).
		if h, level := matchHeading(trimmed); h != "" {
			flushPara()
			flushList()
			fmt.Fprintf(&out, "<h%d>%s</h%d>", level, renderInline(h), level)
			continue
		}

		// List items.
		if item, ok := matchListItem(trimmed); ok {
			flushPara()
			if !inList {
				out.WriteString("<ul>")
				inList = true
			}
			out.WriteString("<li>")
			out.WriteString(renderInline(item))
			out.WriteString("</li>")
			continue
		}

		// Paragraph continuation — close any open list first.
		flushList()
		paraBuf = append(paraBuf, trimmed)
	}

	// EOF flushes.
	if inCodeBlock {
		flushCode()
	}
	flushPara()
	flushList()

	return out.String()
}

// matchHeading returns the heading text and level (1..3) if line is
// an ATX heading, otherwise "" and 0.
func matchHeading(line string) (string, int) {
	switch {
	case strings.HasPrefix(line, "### "):
		return strings.TrimSpace(strings.TrimPrefix(line, "### ")), 3
	case strings.HasPrefix(line, "## "):
		return strings.TrimSpace(strings.TrimPrefix(line, "## ")), 2
	case strings.HasPrefix(line, "# "):
		return strings.TrimSpace(strings.TrimPrefix(line, "# ")), 1
	default:
		return "", 0
	}
}

// matchListItem returns the bullet text if line begins with "- " or
// "* ", otherwise "" and false.
func matchListItem(line string) (string, bool) {
	for _, prefix := range []string{"- ", "* "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):]), true
		}
	}
	return "", false
}

// Inline transforms operate on already-HTML-escaped text so the
// inserted tag characters are not themselves escaped a second time.
// Order matters: code spans first (they protect their contents from
// further interpretation), then links (to capture the bracketed
// shape), then bold.
var (
	reCode = regexp.MustCompile("`([^`]+)`")
	reLink = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	reBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

func renderInline(s string) string {
	s = escapeHTML(s)
	s = reCode.ReplaceAllString(s, "<code>$1</code>")
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		// parts[1] = text (already escaped), parts[2] = url (already escaped)
		if !isSafeLinkURL(parts[2]) {
			// Drop the anchor — render the link text as plain content so
			// model-emitted javascript:/data:/file: schemes can never
			// land as <a href="..."> in Confluence storage.
			return parts[1]
		}
		return fmt.Sprintf(`<a href="%s">%s</a>`, parts[2], parts[1])
	})
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	return s
}

// isSafeLinkURL allow-lists the URL schemes we will render as a real
// anchor. Anything else (javascript:, data:, vbscript:, file:, etc.)
// is dropped to plain text. Inputs have already been HTML-escaped by
// escapeHTML, so we match against the escaped form — the relevant
// scheme prefixes contain no characters that escapeHTML rewrites.
func isSafeLinkURL(url string) bool {
	s := strings.TrimSpace(url)
	if s == "" {
		return false
	}
	switch s[0] {
	case '/', '#':
		return true // relative path or in-page anchor
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "http://"),
		strings.HasPrefix(lower, "https://"),
		strings.HasPrefix(lower, "mailto:"):
		return true
	}
	return false
}

// escapeHTML escapes the five characters that have meaning in XHTML
// attribute and content positions. We do NOT use html.EscapeString
// because we want a stable, minimal output (no &#39; for ', etc.).
func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}
