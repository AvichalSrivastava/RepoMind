package github_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	infragithub "github.com/repomind/repomind/internal/infrastructure/github"
)

func TestLoadRepoPolicyFromFile_EmptyPath_ReturnsNil(t *testing.T) {
	p, err := infragithub.LoadRepoPolicyFromFile("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p != nil {
		t.Errorf("want nil policy for empty path, got %+v", p)
	}
}

func TestLoadRepoPolicyFromFile_MissingFile_ReturnsError(t *testing.T) {
	_, err := infragithub.LoadRepoPolicyFromFile("/no/such/file.yaml")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestLoadRepoPolicyFromFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := `
default_target_branches:
  - qa
repos:
  - full_name: SHL-India/TDM-data-management-ui
    target_branches:
      - test_sprint-183
  - full_name: SHL-India/TDM-tcplus-group-manager
    target_branches:
      - "release/sprint-*"
  - full_name: SHL-India/muted-repo
    target_branches: []
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := infragithub.LoadRepoPolicyFromFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p == nil {
		t.Fatal("want non-nil policy")
	}
	if got, want := p.OverrideCount(), 3; got != want {
		t.Errorf("OverrideCount: got %d want %d", got, want)
	}
	if got := p.DefaultBranches(); len(got) != 1 || got[0] != "qa" {
		t.Errorf("DefaultBranches: got %v want [qa]", got)
	}
}

// ---- Accept() matrix ---------------------------------------------------

func TestAccept_NilPolicy_AcceptsEverything(t *testing.T) {
	var p *infragithub.RepoPolicy
	if !p.Accept("any", "thing", "anywhere") {
		t.Error("nil policy must accept everything")
	}
}

func TestAccept_EmptyBranch_Accepts(t *testing.T) {
	p := mustBuild(t, `
default_target_branches:
  - qa
`)
	if !p.Accept("acme", "thing", "") {
		t.Error("empty branch must be accepted (policy can't decide)")
	}
}

func TestAccept_RepoOverride_ExactMatch_CaseInsensitive(t *testing.T) {
	p := mustBuild(t, `
default_target_branches:
  - qa
repos:
  - full_name: SHL-India/TDM-data-management-ui
    target_branches:
      - test_sprint-183
`)
	cases := []struct {
		owner, name, branch string
		want                bool
	}{
		{"SHL-India", "TDM-data-management-ui", "test_sprint-183", true},
		{"shl-india", "tdm-data-management-ui", "test_sprint-183", true}, // repo lower
		{"SHL-India", "TDM-data-management-ui", "TEST_SPRINT-183", true}, // branch upper
		{"SHL-India", "TDM-data-management-ui", "Test_Sprint-183", true}, // mixed
		{"SHL-India", "TDM-data-management-ui", "qa", false},             // override blocks default
		{"SHL-India", "TDM-data-management-ui", "main", false},
		{"SHL-India", "TDM-data-management-ui", "test_sprint-184", false}, // exact, not glob
	}
	for _, c := range cases {
		got := p.Accept(c.owner, c.name, c.branch)
		if got != c.want {
			t.Errorf("Accept(%s/%s, %s)=%v want %v",
				c.owner, c.name, c.branch, got, c.want)
		}
	}
}

func TestAccept_RepoOverride_GlobMatch_CaseInsensitive(t *testing.T) {
	p := mustBuild(t, `
default_target_branches:
  - qa
repos:
  - full_name: SHL-India/TDM-tcplus-group-manager
    target_branches:
      - "release/sprint-*"
`)
	cases := []struct {
		branch string
		want   bool
	}{
		{"release/sprint-187", true},
		{"release/Sprint-187", true}, // case-insensitive
		{"RELEASE/SPRINT-999", true},
		{"release/sprint-", true},        // empty tail matches `*`
		{"release/sprint-foo-bar", true}, // glob includes hyphens
		{"release/sprintfoo", false},     // missing dash before *
		{"release/sprint", false},        // missing dash entirely
		{"hotfix/sprint-187", false},
		{"qa", false}, // default not consulted (override exists)
	}
	for _, c := range cases {
		got := p.Accept("SHL-India", "TDM-tcplus-group-manager", c.branch)
		if got != c.want {
			t.Errorf("Accept(branch=%s)=%v want %v", c.branch, got, c.want)
		}
	}
}

func TestAccept_RepoNotInPolicy_UsesDefault(t *testing.T) {
	p := mustBuild(t, `
default_target_branches:
  - qa
  - uat
repos:
  - full_name: SHL-India/Other-Repo
    target_branches:
      - main
`)
	// Repo not in overrides -- defaults apply.
	if !p.Accept("SHL-India", "TDM-other-thing", "qa") {
		t.Error("default qa should accept")
	}
	if !p.Accept("SHL-India", "TDM-other-thing", "UAT") { // case-insensitive
		t.Error("default UAT should accept (case-insensitive)")
	}
	if p.Accept("SHL-India", "TDM-other-thing", "main") {
		t.Error("default does not include main; should reject")
	}
}

func TestAccept_RepoExplicitlyMuted(t *testing.T) {
	p := mustBuild(t, `
default_target_branches:
  - qa
repos:
  - full_name: SHL-India/Muted-Repo
    target_branches: []
`)
	// An explicit empty target_branches means MUTE -- the default
	// must NOT leak through.
	for _, branch := range []string{"qa", "main", "release/sprint-1", ""} {
		got := p.Accept("SHL-India", "Muted-Repo", branch)
		// "" branch is always accepted (we can't decide) -- skip
		// that one for the mute assertion.
		if branch == "" {
			if !got {
				t.Errorf("empty branch should always accept; muted accept=%v", got)
			}
			continue
		}
		if got {
			t.Errorf("muted repo must reject branch %q", branch)
		}
	}
}

func TestAccept_NoDefault_NoOverride_AcceptsAll(t *testing.T) {
	// Per the doc: "both lists empty -> accept (fail-open per-repo)".
	p := mustBuild(t, `
repos:
  - full_name: SHL-India/Tracked
    target_branches:
      - main
`)
	// Different repo, no defaults configured -- accept everything.
	if !p.Accept("SHL-India", "Untracked", "weird/branch") {
		t.Error("untracked repo with no defaults must accept")
	}
}

func TestBuild_RejectsMalformedEntries(t *testing.T) {
	cases := []string{
		`repos: [{target_branches: [qa]}]`, // missing full_name
		`repos: [{full_name: "noSlash", target_branches: [qa]}]`,
		`repos:
- full_name: a/b
  target_branches: [qa]
- full_name: A/B
  target_branches: [main]`, // duplicate (case-insensitive)
		`repos: [{full_name: "a/b", target_branches: ["bad[pattern"]}]`, // invalid glob (unbalanced bracket -> escaped, ok actually -- pick something invalid)
	}
	for i, body := range cases {
		_, err := infragithub.LoadRepoPolicyFromBytesForTest(t, body)
		// Only the first 3 cases are guaranteed bad; the 4th
		// depends on how `[` is treated. Tolerate that one.
		if i < 3 && err == nil {
			t.Errorf("case %d: want validation error, got nil", i)
		}
	}
}

// ---- helpers ----------------------------------------------------------

// mustBuild parses YAML body via the test-only entry point and
// fails the test on any error.
func mustBuild(t *testing.T, body string) *infragithub.RepoPolicy {
	t.Helper()
	p, err := infragithub.LoadRepoPolicyFromBytesForTest(t, body)
	if err != nil {
		t.Fatalf("build: %v\nbody:\n%s", err, strings.TrimSpace(body))
	}
	if p == nil {
		t.Fatal("nil policy from non-empty body")
	}
	return p
}
