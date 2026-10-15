package main

import (
	"testing"
	"time"
)

func TestParseFlags_RequiresSprint(t *testing.T) {
	t.Parallel()
	if _, err := parseFlags([]string{}); err == nil {
		t.Fatalf("expected error when -sprint is missing")
	}
}

func TestParseFlags_RejectsZeroSprint(t *testing.T) {
	t.Parallel()
	if _, err := parseFlags([]string{"-sprint", "0"}); err == nil {
		t.Fatalf("expected error when -sprint=0")
	}
	if _, err := parseFlags([]string{"-sprint", "-1"}); err == nil {
		t.Fatalf("expected error when -sprint is negative")
	}
}

func TestParseFlags_RejectsBadMaxPerRepo(t *testing.T) {
	t.Parallel()
	if _, err := parseFlags([]string{"-sprint", "1", "-max-per-repo", "0"}); err == nil {
		t.Fatalf("expected error when -max-per-repo=0")
	}
}

func TestParseFlags_HappyPathDefaults(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{"-sprint", "3974"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.sprintJiraID != 3974 {
		t.Errorf("sprint jira id: got %d, want 3974", f.sprintJiraID)
	}
	if f.maxPerRepo != 500 {
		t.Errorf("max-per-repo default: got %d, want 500", f.maxPerRepo)
	}
	if f.dryRun {
		t.Errorf("dry-run default: got true, want false")
	}
}

func TestParseFlags_AllOverrides(t *testing.T) {
	t.Parallel()
	f, err := parseFlags([]string{
		"-sprint", "1",
		"-repos", "SHL-India/A,SHL-India/B",
		"-since", "2026-06-01T00:00:00Z",
		"-until", "2026-07-01T00:00:00Z",
		"-max-per-repo", "10",
		"-dry-run",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.reposCSV != "SHL-India/A,SHL-India/B" {
		t.Errorf("reposCSV: got %q", f.reposCSV)
	}
	if !f.dryRun {
		t.Errorf("dry-run: got false, want true")
	}
	if f.maxPerRepo != 10 {
		t.Errorf("max-per-repo: got %d, want 10", f.maxPerRepo)
	}
}

func TestResolveWindow_FromSprintDates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	w, err := resolveWindow(&flags{}, &start, &end)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !w.start.Equal(start) || !w.end.Equal(end) {
		t.Errorf("window: got [%s, %s], want [%s, %s]", w.start, w.end, start, end)
	}
}

func TestResolveWindow_FlagsOverrideSprint(t *testing.T) {
	t.Parallel()
	sStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sEnd := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	f := &flags{
		since: "2026-06-01T00:00:00Z",
		until: "2026-07-01T00:00:00Z",
	}
	w, err := resolveWindow(f, &sStart, &sEnd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.start.Month() != time.June || w.end.Month() != time.July {
		t.Errorf("expected flag overrides to win; got [%s, %s]", w.start, w.end)
	}
}

func TestResolveWindow_ErrorsWhenBothMissing(t *testing.T) {
	t.Parallel()
	if _, err := resolveWindow(&flags{}, nil, nil); err == nil {
		t.Fatalf("expected error when sprint has no dates and no flag overrides")
	}
}

func TestResolveWindow_ErrorsOnBadRFC3339(t *testing.T) {
	t.Parallel()
	if _, err := resolveWindow(&flags{since: "not-a-time"}, nil, nil); err == nil {
		t.Fatalf("expected -since parse error")
	}
	start := time.Now()
	end := start.Add(time.Hour)
	if _, err := resolveWindow(&flags{until: "still-not-a-time"}, &start, &end); err == nil {
		t.Fatalf("expected -until parse error")
	}
}

func TestResolveWindow_RejectsStartAfterEnd(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := resolveWindow(&flags{}, &start, &end); err == nil {
		t.Fatalf("expected error when start > end")
	}
}

func TestResolveWindow_RejectsStartEqualsEnd(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := resolveWindow(&flags{}, &t0, &t0); err == nil {
		t.Fatalf("expected error when start == end")
	}
}

func TestTrim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"this string is way too long", 10, "this stri…"},
	}
	for _, tc := range cases {
		if got := trim(tc.in, tc.n); got != tc.want {
			t.Errorf("trim(%q, %d): got %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
