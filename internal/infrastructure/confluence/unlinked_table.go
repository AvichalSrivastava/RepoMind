package confluence

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/repomind/repomind/internal/application/publish"
)

// The unlinked-PRs section is a fixed-name region on the target page,
// distinct from the release-log-table section. Unlike the release
// log (which append-preserves per-row operator edits), this section
// is REPLACED wholesale on every publish -- the caller's slice is
// authoritative, so a PR that gets a ticket added later (author
// amends title, next publish) naturally drops out.
const unlinkedPRsSectionName = "unlinked-prs-table"

// unlinkedTableStyleHeader / …Cell mirror the release-log palette but
// use a lower-emphasis, warning-tinted header so operators can tell
// the two tables apart at a glance.
const (
	unlinkedStyleHeader = `background-color: #de350b; color: #ffffff; font-weight: 600; padding: 6px 10px; text-align: left; border: 1px solid #7a1a05;`
	unlinkedStyleCell   = `padding: 6px 10px; vertical-align: top; border: 1px solid #dfe1e6;`
	unlinkedStyleEven   = `background-color: #ffebe6;`
	unlinkedStyleOdd    = `background-color: #ffffff;`
)

// unlinkedSectionExistsRe detects whether the unlinked-PRs section
// markers are already present on the page. Used by the publisher to
// decide whether an empty input should CLEAR an existing section or
// leave the page alone.
var unlinkedSectionExistsRe = regexp.MustCompile(
	`<!--\s*repomind:section:start\s+name="` + regexp.QuoteMeta(unlinkedPRsSectionName) + `"\s*-->`,
)

// hasUnlinkedSection reports whether the page body already carries
// an unlinked-PRs section (start marker present).
func hasUnlinkedSection(body string) bool {
	return unlinkedSectionExistsRe.MatchString(body)
}

// renderUnlinkedPRsSection produces the XHTML body that lives BETWEEN
// the unlinked-PRs section markers. Layout:
//
//	<h2>Unlinked PRs</h2>
//	<p><em>These PRs were merged during the sprint window but have
//	no Jira ticket reference in the title, branch, body, or commit
//	messages. Add a ticket ID to the PR title and re-publish, or
//	link the PR manually.</em></p>
//	<table>
//	  <thead>
//	    <tr><th>Repo</th><th>PR</th><th>Title</th><th>Author</th>
//	        <th>Head Branch</th><th>Merged (UTC)</th></tr>
//	  </thead>
//	  <tbody>…rows sorted by merged_at DESC…</tbody>
//	</table>
//
// Returns "" when rows is empty -- callers use that to decide
// whether to skip the section entirely on a first publish.
func renderUnlinkedPRsSection(rows []publish.UnlinkedPRRef) string {
	if len(rows) == 0 {
		return ""
	}

	// Stable sort: newest merges first; ties broken by repo then PR#.
	clone := append([]publish.UnlinkedPRRef(nil), rows...)
	sort.SliceStable(clone, func(i, j int) bool {
		if !clone[i].MergedAt.Equal(clone[j].MergedAt) {
			return clone[i].MergedAt.After(clone[j].MergedAt)
		}
		if clone[i].RepoFullName != clone[j].RepoFullName {
			return clone[i].RepoFullName < clone[j].RepoFullName
		}
		return clone[i].Number < clone[j].Number
	})

	var b strings.Builder
	b.WriteString(`<h2>Unlinked PRs</h2>`)
	b.WriteString(`<p><em>These PRs were merged during the sprint window but have no Jira ticket reference in the title, branch, body, or commit messages. Add a ticket ID to the PR title and re-publish, or link the PR manually.</em></p>`)

	b.WriteString(`<table data-repomind-unlinked-prs="1" style="border-collapse: collapse; width: 100%; border: 2px solid #7a1a05;">`)
	b.WriteString(`<colgroup>`)
	b.WriteString(`<col style="width: 200px;"/>`) // repo
	b.WriteString(`<col style="width: 60px;"/>`)  // pr
	b.WriteString(`<col/>`)                       // title
	b.WriteString(`<col style="width: 130px;"/>`) // author
	b.WriteString(`<col style="width: 200px;"/>`) // head branch
	b.WriteString(`<col style="width: 140px;"/>`) // merged at
	b.WriteString(`</colgroup>`)

	b.WriteString(`<thead><tr>`)
	fmt.Fprintf(&b, `<th style="%s">Repo</th>`, unlinkedStyleHeader)
	fmt.Fprintf(&b, `<th style="%s">PR</th>`, unlinkedStyleHeader)
	fmt.Fprintf(&b, `<th style="%s">Title</th>`, unlinkedStyleHeader)
	fmt.Fprintf(&b, `<th style="%s">Author</th>`, unlinkedStyleHeader)
	fmt.Fprintf(&b, `<th style="%s">Head Branch</th>`, unlinkedStyleHeader)
	fmt.Fprintf(&b, `<th style="%s">Merged (UTC)</th>`, unlinkedStyleHeader)
	b.WriteString(`</tr></thead>`)

	b.WriteString(`<tbody>`)
	for i, r := range clone {
		bg := unlinkedStyleOdd
		if i%2 == 1 {
			bg = unlinkedStyleEven
		}
		fmt.Fprintf(&b, `<tr style="%s">`, bg)

		// Repo: display the short name (drop owner) for compactness.
		fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, escapeHTML(stripOwner(r.RepoFullName)))

		// PR # -- linked when URL is safe, else plain text.
		prCell := fmt.Sprintf("#%d", r.Number)
		if url := strings.TrimSpace(r.URL); url != "" && isSafeLinkURL(url) {
			prCell = fmt.Sprintf(`<a href="%s">#%d</a>`, escapeHTML(url), r.Number)
			fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, prCell)
		} else {
			fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, escapeHTML(prCell))
		}

		fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, escapeHTML(r.Title))
		fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, escapeHTML(r.Author))
		fmt.Fprintf(&b, `<td style="%s"><code>%s</code></td>`, unlinkedStyleCell, escapeHTML(r.HeadBranch))

		mergedStr := ""
		if !r.MergedAt.IsZero() {
			mergedStr = r.MergedAt.UTC().Format("02 Jan 2006 15:04")
		}
		fmt.Fprintf(&b, `<td style="%s">%s</td>`, unlinkedStyleCell, escapeHTML(mergedStr))

		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)

	return b.String()
}
