package orchestration

import (
	"testing"

	"github.com/repomind/repomind/internal/domain/shared"
)

func TestExtractTicketKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sources []string
		want    []string // canonical uppercased keys in first-seen order
	}{
		{
			name:    "nil sources",
			sources: nil,
			want:    nil,
		},
		{
			name:    "empty strings only",
			sources: []string{"", "", ""},
			want:    nil,
		},
		{
			name:    "single key in title",
			sources: []string{"SIS-9505 fix report export"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "case insensitive lowercase in branch",
			sources: []string{"feature/sis-9505-fix"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "mixed case is uppercased",
			sources: []string{"Sis-9505 and sIS-9506"},
			want:    []string{"SIS-9505", "SIS-9506"},
		},
		{
			name:    "path segment branch",
			sources: []string{"feature/SIS-9505-report-fix"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "bugfix prefix branch",
			sources: []string{"bugfix/MFS-100-null-pointer"},
			want:    []string{"MFS-100"},
		},
		{
			name:    "url path",
			sources: []string{"see https://jira.example.com/browse/SIS-9505 for details"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "parenthesised",
			sources: []string{"landed fix (SIS-9505) into main"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "trailing punctuation",
			sources: []string{"SIS-9505."},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "multiple projects mixed",
			sources: []string{"MFS-100 TDM-200 SIS-300"},
			want:    []string{"MFS-100", "TDM-200", "SIS-300"},
		},
		{
			name:    "dedup within one source",
			sources: []string{"SIS-1 fixes and re-fixes SIS-1"},
			want:    []string{"SIS-1"},
		},
		{
			name: "dedup across sources - first source wins order",
			sources: []string{
				"SIS-9505 in title",
				"body mentions SIS-9506 and SIS-9505 again",
				"feature/SIS-9506-branch",
			},
			want: []string{"SIS-9505", "SIS-9506"},
		},
		{
			name:    "letters run into project prefix is greedy match",
			sources: []string{"someSIS-9505 word"},
			// Greedy: matches "someSIS-9505" -> SOMESIS-9505. This is
			// a valid Jira-shaped key syntactically; downstream
			// PullRequests.ListByTickets(sprintTickets) filters out
			// keys not in the current sprint, so false positives are
			// harmless. Documented behaviour, not a bug.
			want: []string{"SOMESIS-9505"},
		},
		{
			name:    "false positive - key glued to trailing letters",
			sources: []string{"SIS-9505abc"},
			want:    nil, // \b prevents match before word char "a"
		},
		{
			name:    "boundary - start of string",
			sources: []string{"SIS-1 opens"},
			want:    []string{"SIS-1"},
		},
		{
			name:    "boundary - end of string",
			sources: []string{"closes SIS-1"},
			want:    []string{"SIS-1"},
		},
		{
			name:    "hyphen surrounded by punctuation",
			sources: []string{"[SIS-9505]"},
			want:    []string{"SIS-9505"},
		},
		{
			name:    "underscore in project prefix",
			sources: []string{"MY_KEY-100 rocks"},
			want:    []string{"MY_KEY-100"},
		},
		{
			name:    "not a jira key - just number",
			sources: []string{"issue 1234"},
			want:    nil,
		},
		{
			name:    "commit-style body",
			sources: []string{"SIS-9505: fix report export\n\nAlso touches MFS-100 for schema."},
			want:    []string{"SIS-9505", "MFS-100"},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractTicketKeys(tc.sources...)
			gotStrs := keysToStrings(got)
			if len(got) != len(tc.want) {
				t.Fatalf("count mismatch: want %v, got %v", tc.want, gotStrs)
			}
			for i := range got {
				if gotStrs[i] != tc.want[i] {
					t.Errorf("[%d]: want %q, got %q", i, tc.want[i], gotStrs[i])
				}
			}
		})
	}
}

func TestExtractTicketKeys_HeadBranchIsTheImportantOne(t *testing.T) {
	t.Parallel()
	// Exact scenario the user reported:
	// title/body do not name the ticket, only the head branch does.
	title := "Report export tweak"
	body := "No ticket in body."
	head := "feature/SIS-9505-report-export-tweak"

	got := extractTicketKeys(title, body, head)
	if len(got) != 1 || got[0].String() != "SIS-9505" {
		t.Fatalf("want [SIS-9505] via head branch only; got %v", keysToStrings(got))
	}
}

func TestExtractTicketKeys_StableOrder(t *testing.T) {
	t.Parallel()
	// Order must be deterministic (first-seen) so downstream hashing
	// and rendering stay reproducible. Prefixes need 2+ chars to
	// satisfy the domain regex ([A-Z][A-Z0-9_]{1,15}).
	got := extractTicketKeys("ZZ-9 first", "AA-1 second", "ZZ-9 dup", "MM-5 third")
	want := []string{"ZZ-9", "AA-1", "MM-5"}
	gotStrs := keysToStrings(got)
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, gotStrs)
	}
	for i := range got {
		if gotStrs[i] != want[i] {
			t.Errorf("[%d]: want %q, got %q", i, want[i], gotStrs[i])
		}
	}
}

func keysToStrings(keys []shared.TicketKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.String()
	}
	return out
}
