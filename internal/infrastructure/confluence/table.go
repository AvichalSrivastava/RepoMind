package confluence

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/repomind/repomind/internal/application/publish"
)

// The release-log-table section is one fixed-name region on the
// target page. The publisher locates it across publishes for ANY
// sprint and inserts new rows (one per ticket) at the top of <tbody>.
const releaseLogSectionName = "release-log-table"

// rowAnchorRe matches the per-row HTML-comment anchor: (sprint UUID,
// ticket key). Confluence preserves HTML comments verbatim in storage
// XHTML, so the anchor survives operator hand-edits to any visible
// cell of the row.
var rowAnchorRe = regexp.MustCompile(
	`<!--\s*repomind:row\s+sprint=([0-9a-fA-F-]{36})\s+ticket=([A-Za-z][A-Za-z0-9_]{1,15}-[0-9]+)\s*-->`,
)

// Visual styling. Confluence Cloud honours `style` on
// <th>/<tr>/<td> for the conservative attributes below; macros and
// most `data-*` attributes are stripped, but inline colour / padding
// / border survive in practice.
const (
	styleHeader      = `background-color: #0052cc; color: #ffffff; font-weight: 600; padding: 8px 10px; text-align: left; border: 1px solid #001a4d;`
	styleEvenBg      = `background-color: #f4f5f7;`
	styleOddBg       = `background-color: #ffffff;`
	styleCell        = `padding: 8px 10px; vertical-align: top; border-left: 1px solid #dfe1e6; border-right: 1px solid #dfe1e6;`
	styleWithinGroup = `border-top: 1px solid #5e6c84;`    // between rows of the SAME sprint (medium grey)
	styleSprintTop   = `border-top: 4px solid #172b4d;`    // first row of a NEW sprint (dark navy)
	styleSprintBot   = `border-bottom: 4px solid #172b4d;` // last row of a sprint group (dark navy)
)

// tableHeaderXHTML is emitted on the first publish; thereafter we
// just insert new <tr> rows at the top of <tbody>.
const tableHeaderXHTML = `<table data-repomind-release-log="1" style="border-collapse: collapse; width: 100%; border: 2px solid #172b4d;">` +
	`<colgroup>` +
	`<col style="width: 110px;"/>` + // release planned date
	`<col style="width: 100px;"/>` + // sprint start date
	`<col style="width: 80px;"/>` + // sprint id
	`<col style="width: 110px;"/>` + // ticket id
	`<col/>` + // description
	`<col style="width: 110px;"/>` + // Dev owner
	`<col style="width: 110px;"/>` + // QA Owner
	`<col/>` + // Release Details
	`<col style="width: 100px;"/>` + // sprint end date
	`</colgroup>` +
	`<thead><tr>` +
	`<th style="` + styleHeader + `">Release Planned Date</th>` +
	`<th style="` + styleHeader + `">Sprint Start</th>` +
	`<th style="` + styleHeader + `">Sprint ID</th>` +
	`<th style="` + styleHeader + `">Ticket ID</th>` +
	`<th style="` + styleHeader + `">Description</th>` +
	`<th style="` + styleHeader + `">Dev Owner</th>` +
	`<th style="` + styleHeader + `">QA Owner</th>` +
	`<th style="` + styleHeader + `">Release Details</th>` +
	`<th style="` + styleHeader + `">Sprint End</th>` +
	`</tr></thead>` +
	`<tbody>`

const tableFooterXHTML = `</tbody></table>`

// rowKind classifies a row's vertical position inside its sprint
// group so the renderer can pick the right borders.
type rowKind int

const (
	rowFirstOfSprint  rowKind = iota // first row of a NEW sprint -> thick top border
	rowMiddleOfSprint                // dashed top border (intra-sprint divider)
	rowSoloOfSprint                  // only row for the sprint -> thick top AND bottom
	rowLastOfSprint                  // dashed top + thick bottom
)

// renderTicketRowXHTML produces a single <tr>...</tr>. Columns:
//
//  1. Release Planned Date  2. Sprint Start  3. Sprint ID
//  4. Ticket ID  5. Description  6. Dev Owner  7. QA Owner
//  8. Release Details  9. Sprint End
//
// rowBgStyle picks zebra striping; kind picks the sprint-group
// borders.
func renderTicketRowXHTML(row publish.ReleaseLogRow, rowBgStyle string, kind rowKind) string {
	var b strings.Builder

	// Compose per-row style: zebra bg + sprint-group borders.
	var borderStyle string
	switch kind {
	case rowFirstOfSprint:
		borderStyle = styleSprintTop
	case rowSoloOfSprint:
		borderStyle = styleSprintTop + styleSprintBot
	case rowLastOfSprint:
		borderStyle = styleWithinGroup + styleSprintBot
	default:
		borderStyle = styleWithinGroup
	}
	fmt.Fprintf(&b,
		`<tr style="%s%s"><!-- repomind:row sprint=%s ticket=%s -->`,
		rowBgStyle, borderStyle, row.SprintID.String(), row.TicketKey,
	)

	td := func(text string) {
		fmt.Fprintf(&b, `<td style="%s">%s</td>`, styleCell, escapeHTML(text))
	}
	rawTD := func(html string) {
		fmt.Fprintf(&b, `<td style="%s">%s</td>`, styleCell, html)
	}

	// 1. Release Planned Date (operator-owned; left empty so leads
	// can decide the planned release date in Confluence). The per-row
	// anchor preserves their edit across re-publishes.
	td(formatDateOr(row.ReleaseDate, ""))

	// 2. Sprint Start.
	td(formatDateOr(row.SprintStartDate, ""))

	// 3. Sprint ID -- short human-friendly number (e.g. 185), with
	// the underlying Jira sprint id as a fallback if the name didn't
	// parse to a number.
	sid := ""
	switch {
	case row.SprintNumber > 0:
		sid = fmt.Sprintf("%d", row.SprintNumber)
	case row.SprintJiraID > 0:
		sid = fmt.Sprintf("%d", row.SprintJiraID)
	}
	td(sid)

	// 4. Ticket ID.
	rawTD(renderTicket(row.TicketKey, row.JiraBaseURL))

	// 5. Description (operator-editable; empty by default).
	td(row.Description)

	// 6. Dev Owner.
	td(strings.Join(dedupSorted(row.DevOwners), ", "))

	// 7. QA Owner -- populated from the Jira "QA Owner" custom
	// field via the release-notes handler. Falls back to &nbsp;
	// when the field is unset on the issue (or the field id is
	// not configured for this Jira instance).
	if q := strings.Join(dedupSorted(row.QAOwners), ", "); q != "" {
		td(q)
	} else {
		rawTD(`&nbsp;`)
	}

	// 8. Release Details.
	rawTD(renderPRsCell(row.PRs))

	// 9. Sprint End.
	td(formatDateOr(row.SprintEndDate, ""))

	b.WriteString(`</tr>`)
	return b.String()
}

// renderPRsCell renders the PRs list as one paragraph per PR in the
// form: `repo-short - https://github.com/.../pull/N`.
func renderPRsCell(prs []publish.PRRef) string {
	if len(prs) == 0 {
		return `<p><em>No merged PRs.</em></p>`
	}
	clone := append([]publish.PRRef(nil), prs...)
	sort.SliceStable(clone, func(i, j int) bool {
		if clone[i].RepoFullName != clone[j].RepoFullName {
			return clone[i].RepoFullName < clone[j].RepoFullName
		}
		return clone[i].Number < clone[j].Number
	})
	var b strings.Builder
	for _, pr := range clone {
		short := stripOrgPrefix(stripOwner(pr.RepoFullName))
		fmt.Fprintf(&b, `<p>%s - %s</p>`, escapeHTML(short), renderPRURL(pr))
	}
	return b.String()
}

// renderPRURL emits the PR URL as visible link text. Falls back to
// `PR #<n>` when the URL is missing or fails the scheme allow-list.
func renderPRURL(pr publish.PRRef) string {
	url := strings.TrimSpace(pr.URL)
	if url == "" || !isSafeLinkURL(url) {
		return escapeHTML(fmt.Sprintf("PR #%d", pr.Number))
	}
	return fmt.Sprintf(`<a href="%s">%s</a>`, escapeHTML(url), escapeHTML(url))
}

// renderTicket renders the ticket key as a link when jiraBaseURL is
// set, otherwise as escaped plain text.
func renderTicket(key, jiraBaseURL string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "&nbsp;"
	}
	jiraBaseURL = strings.TrimRight(strings.TrimSpace(jiraBaseURL), "/")
	if jiraBaseURL == "" {
		return escapeHTML(key)
	}
	url := jiraBaseURL + "/browse/" + key
	if !isSafeLinkURL(url) {
		return escapeHTML(key)
	}
	return fmt.Sprintf(`<a href="%s"><strong>%s</strong></a>`, escapeHTML(url), escapeHTML(key))
}

// dedupSorted returns a sorted, de-duplicated copy of in.
func dedupSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func stripOwner(repoFullName string) string {
	if i := strings.Index(repoFullName, "/"); i >= 0 && i < len(repoFullName)-1 {
		return repoFullName[i+1:]
	}
	return repoFullName
}

var orgPrefixRE = regexp.MustCompile(`^[A-Z]{2,6}-`)

func stripOrgPrefix(name string) string {
	return orgPrefixRE.ReplaceAllString(name, "")
}

func formatDateOr(t time.Time, fallback string) string {
	if t.IsZero() {
		return fallback
	}
	return t.UTC().Format("02 Jan 2006")
}

// upsertReleaseLogRows splices ticket rows into the release-log-table
// section. New rows are inserted at the top of <tbody> (newest sprint
// first); rows whose (SprintID, TicketKey) anchor already exists are
// skipped so operator hand-edits survive.
//
// When forceRefresh is true, existing anchor rows are stripped from
// the table before insertion so the caller's freshly-built values
// replace them. Used to backfill a column (e.g. QA Owner) added
// after the original publish. Operator hand-edits to affected rows
// are lost -- reserved for explicit operator-invoked refreshes.
//
// All rows passed in are expected to belong to ONE NEW sprint (per
// the per-publish handler contract); they share the same SprintID
// and get sprint-group border styling within the batch.
func upsertReleaseLogRows(body string, rows []publish.ReleaseLogRow, forceRefresh bool) (string, int, error) {
	if len(rows) == 0 {
		return body, 0, nil
	}
	for i, r := range rows {
		if r.SprintID == uuid.Nil {
			return "", 0, fmt.Errorf("confluence: row %d missing SprintID", i)
		}
		if strings.TrimSpace(r.TicketKey) == "" {
			return "", 0, fmt.Errorf("confluence: row %d missing TicketKey", i)
		}
	}

	startRe := regexp.MustCompile(
		`<!--\s*repomind:section:start\s+name="` + regexp.QuoteMeta(releaseLogSectionName) + `"\s*-->`,
	)
	endRe := regexp.MustCompile(
		`<!--\s*repomind:section:end\s+name="` + regexp.QuoteMeta(releaseLogSectionName) + `"\s*-->`,
	)

	starts := startRe.FindAllStringIndex(body, -1)
	ends := endRe.FindAllStringIndex(body, -1)

	if len(starts) == 0 && len(ends) == 0 {
		// First publish: build the full table from scratch.
		fullTable := tableHeaderXHTML + renderRowsXHTML(rows, 0) + tableFooterXHTML
		newBody, err := replaceSection(body, releaseLogSectionName, fullTable)
		return newBody, len(rows), err
	}
	if len(starts) != 1 || len(ends) != 1 || starts[0][1] > ends[0][0] {
		return "", 0, errSectionInconsistent
	}

	innerStart := starts[0][1]
	innerEnd := ends[0][0]
	inner := body[innerStart:innerEnd]

	// Build the anchor set of rows we're about to insert (used for
	// both the skip check and the force-refresh strip).
	incoming := make(map[string]bool, len(rows))
	for _, r := range rows {
		incoming[strings.ToLower(r.SprintID.String()+"|"+r.TicketKey)] = true
	}

	// When force-refreshing, strip any existing <tr> whose anchor
	// matches an incoming row so the fresh values replace them.
	if forceRefresh {
		inner = stripMatchingRows(inner, incoming)
	}

	// Skip rows whose anchor is already on the page. (After a
	// force-refresh strip, no anchors match, so nothing is skipped.)
	present := make(map[string]bool)
	for _, m := range rowAnchorRe.FindAllStringSubmatch(inner, -1) {
		present[strings.ToLower(m[1]+"|"+m[2])] = true
	}
	toAdd := make([]publish.ReleaseLogRow, 0, len(rows))
	for _, r := range rows {
		key := strings.ToLower(r.SprintID.String() + "|" + r.TicketKey)
		if present[key] {
			continue
		}
		toAdd = append(toAdd, r)
	}
	if len(toAdd) == 0 {
		return body, 0, nil
	}

	// Existing-row count (excluding header) determines zebra parity.
	existingRows := strings.Count(inner, "<tr") - strings.Count(inner, "<thead><tr")

	newXHTML := renderRowsXHTML(toAdd, existingRows)

	tbodyIdx := strings.Index(inner, "<tbody>")
	var newInner string
	if tbodyIdx == -1 {
		// Section exists but no recognisable table -- rebuild.
		newInner = tableHeaderXHTML + newXHTML + tableFooterXHTML
	} else {
		insertAt := tbodyIdx + len("<tbody>")
		newInner = inner[:insertAt] + newXHTML + inner[insertAt:]
	}
	newBody, err := replaceSection(body, releaseLogSectionName, strings.TrimSpace(newInner))
	return newBody, len(toAdd), err
}

// stripMatchingRows removes every <tr>...</tr> in inner whose
// row-anchor comment matches a key in the incoming set. Non-matching
// rows and non-row markup (thead, tbody wrappers) are preserved
// byte-for-byte.
func stripMatchingRows(inner string, incoming map[string]bool) string {
	// Find every <tr ...> ... </tr> whose opening tag carries a
	// repomind:row comment matching an incoming anchor. Using a
	// non-greedy regex here rather than a full HTML parser is
	// acceptable because we control the emitted structure -- rows
	// never nest and never contain "</tr>" in their content.
	stripRe := regexp.MustCompile(`(?is)<tr[^>]*>\s*<!--\s*repomind:row\s+sprint=([0-9a-fA-F-]{36})\s+ticket=([A-Za-z][A-Za-z0-9_]{1,15}-[0-9]+)\s*-->.*?</tr>`)
	return stripRe.ReplaceAllStringFunc(inner, func(match string) string {
		m := stripRe.FindStringSubmatch(match)
		if len(m) != 3 {
			return match
		}
		key := strings.ToLower(m[1] + "|" + m[2])
		if incoming[key] {
			return ""
		}
		return match
	})
}

// renderRowsXHTML walks `rows` IN GIVEN ORDER and emits the <tr>s
// with sprint-group borders + zebra striping. parityOffset shifts
// the zebra colour so the new batch interleaves with existing rows
// on the page.
//
// Sprint grouping rule: rows whose SprintID differs from the
// previous row's start a new group; first row of a group gets the
// thick top border, last gets the thick bottom border. A solo
// row gets both. The grouping is computed in render order (which
// the caller controls).
func renderRowsXHTML(rows []publish.ReleaseLogRow, parityOffset int) string {
	var b strings.Builder
	for i, r := range rows {
		kind := rowMiddleOfSprint
		prevDifferent := i == 0 || rows[i-1].SprintID != r.SprintID
		nextDifferent := i == len(rows)-1 || rows[i+1].SprintID != r.SprintID
		switch {
		case prevDifferent && nextDifferent:
			kind = rowSoloOfSprint
		case prevDifferent:
			kind = rowFirstOfSprint
		case nextDifferent:
			kind = rowLastOfSprint
		}
		bg := styleOddBg
		if (i+parityOffset)%2 == 0 {
			bg = styleEvenBg
		}
		b.WriteString(renderTicketRowXHTML(r, bg, kind))
	}
	return b.String()
}
