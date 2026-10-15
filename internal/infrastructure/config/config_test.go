package config_test

import (
	"strings"
	"testing"

	"github.com/repomind/repomind/internal/infrastructure/config"
)

// setEnv sets a minimum-viable set of env vars for Load() to succeed.
// Tests can override any of them via t.Setenv after calling this.
func setEnv(t *testing.T) {
	t.Helper()
	t.Setenv("REPOMIND_POSTGRES_DSN", "postgres://u:p@localhost:5432/db?sslmode=disable")
	t.Setenv("REPOMIND_JIRA_BASE_URL", "https://shl.atlassian.net")
	t.Setenv("REPOMIND_JIRA_EMAIL", "a@b.com")
	t.Setenv("REPOMIND_JIRA_API_TOKEN", "tok")
	t.Setenv("REPOMIND_JIRA_PROJECT_KEY", "SIS")
	t.Setenv("REPOMIND_JIRA_BOARD_ID", "187")
	t.Setenv("REPOMIND_JIRA_TEAM_NAME", "Marvelous")
	t.Setenv("REPOMIND_GITHUB_REPOS", "SHL-India/TDM-A, SHL-India/MFS-B")
	t.Setenv("REPOMIND_AI_API_KEY", "sk-test")
}

func TestLoad_HappyPath(t *testing.T) {
	setEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Jira.ProjectKey != "SIS" || cfg.Jira.BoardID != 187 || cfg.Jira.TeamName != "Marvelous" {
		t.Errorf("jira fields = %+v", cfg.Jira)
	}
	if cfg.GitHub.Repos == "" {
		t.Error("github.repos should be populated")
	}
	if got := cfg.Jira.ResolvedSprintNamePrefix(); got != "Marvelous_Sprint_" {
		t.Errorf("default sprint prefix = %q want Marvelous_Sprint_", got)
	}
}

func TestLoad_OverrideSprintPrefix(t *testing.T) {
	setEnv(t)
	t.Setenv("REPOMIND_JIRA_SPRINT_NAME_PREFIX", "Custom_")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Jira.ResolvedSprintNamePrefix(); got != "Custom_" {
		t.Errorf("override sprint prefix = %q", got)
	}
}

func TestLoad_RejectsMissingJiraFields(t *testing.T) {
	cases := []struct {
		name string
		key  string   // env var to clear
		want []string // any of these substrings is acceptable in the error
	}{
		{"missing project key", "REPOMIND_JIRA_PROJECT_KEY", []string{"project_key", "PROJECT_KEY"}},
		{"missing board id", "REPOMIND_JIRA_BOARD_ID", []string{"board_id", "BOARD_ID", "BoardID"}},
		{"missing team name", "REPOMIND_JIRA_TEAM_NAME", []string{"team_name", "TEAM_NAME"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t)
			t.Setenv(tc.key, "")
			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected error")
			}
			matched := false
			for _, w := range tc.want {
				if strings.Contains(err.Error(), w) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("error %q does not mention any of %v", err.Error(), tc.want)
			}
		})
	}
}

func TestLoad_RejectsBadGitHubRepos(t *testing.T) {
	setEnv(t)
	t.Setenv("REPOMIND_GITHUB_REPOS", "noslash,another-bad")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "github.repos") {
		t.Fatalf("expected github.repos error, got %v", err)
	}
}

func TestLoad_RejectsEmptyGitHubRepos(t *testing.T) {
	setEnv(t)
	t.Setenv("REPOMIND_GITHUB_REPOS", "")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "github.repos") {
		t.Fatalf("expected github.repos error, got %v", err)
	}
}

func TestJiraAdapter_Mapping(t *testing.T) {
	setEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	jc := cfg.JiraAdapter()
	if jc.BaseURL != "https://shl.atlassian.net" {
		t.Errorf("base url = %q", jc.BaseURL)
	}
	if string(jc.AuthMode) != "basic" {
		t.Errorf("auth mode = %q", jc.AuthMode)
	}
	if jc.Email != "a@b.com" || jc.APIToken != "tok" {
		t.Errorf("creds = %+v", jc)
	}
}

func TestGitHubAdapter_Mapping(t *testing.T) {
	setEnv(t)
	t.Setenv("REPOMIND_GITHUB_APP_ID", "4150842")
	t.Setenv("REPOMIND_GITHUB_INSTALLATION_ID", "99")
	t.Setenv("REPOMIND_GITHUB_PRIVATE_KEY_PATH", "/secrets/k.pem")
	t.Setenv("REPOMIND_GITHUB_WEBHOOK_SECRET", "wh")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gc, err := cfg.GitHubAdapter()
	if err != nil {
		t.Fatalf("github adapter: %v", err)
	}
	if gc.AppID != 4150842 || gc.InstallationID != 99 {
		t.Errorf("ids = %+v", gc)
	}
	if gc.PrivateKeyPath != "/secrets/k.pem" {
		t.Errorf("key path = %q", gc.PrivateKeyPath)
	}
	if len(gc.Repos) != 2 ||
		gc.Repos[0].Owner != "SHL-India" || gc.Repos[0].Name != "TDM-A" ||
		gc.Repos[1].Owner != "SHL-India" || gc.Repos[1].Name != "MFS-B" {
		t.Errorf("repos = %+v", gc.Repos)
	}
}

// setConfluenceEnv layers a minimum-viable Confluence env on top of
// setEnv. It is used by tests that exercise HasConfluence /
// ConfluenceAdapter / the all-or-nothing coherence validator.
func setConfluenceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("REPOMIND_CONFLUENCE_BASE_URL", "https://acme.atlassian.net/wiki")
	t.Setenv("REPOMIND_CONFLUENCE_EMAIL", "ops@acme.com")
	t.Setenv("REPOMIND_CONFLUENCE_API_TOKEN", "tok")
	t.Setenv("REPOMIND_CONFLUENCE_PAGE_ID", "12345")
}

func TestHasConfluence_FalseWhenUnset(t *testing.T) {
	setEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HasConfluence() {
		t.Errorf("HasConfluence = true with no confluence env")
	}
}

func TestHasConfluence_TrueWhenAllSet(t *testing.T) {
	setEnv(t)
	setConfluenceEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.HasConfluence() {
		t.Errorf("HasConfluence = false with full confluence env")
	}
}

func TestLoad_RejectsPartialConfluenceConfig(t *testing.T) {
	// Setting only the base URL must trip the all-or-nothing rule.
	setEnv(t)
	t.Setenv("REPOMIND_CONFLUENCE_BASE_URL", "https://acme.atlassian.net/wiki")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "confluence") {
		t.Fatalf("expected confluence coherence error, got %v", err)
	}
}

func TestLoad_RejectsBadConfluenceBaseURL(t *testing.T) {
	setEnv(t)
	setConfluenceEnv(t)
	t.Setenv("REPOMIND_CONFLUENCE_BASE_URL", "ftp://nope")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "confluence.base_url") {
		t.Fatalf("expected confluence.base_url error, got %v", err)
	}
}

func TestLoad_RejectsNegativeConfluenceMaxRetries(t *testing.T) {
	setEnv(t)
	setConfluenceEnv(t)
	t.Setenv("REPOMIND_CONFLUENCE_MAX_RETRIES", "-1")
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "confluence.max_retries") {
		t.Fatalf("expected confluence.max_retries error, got %v", err)
	}
}

func TestConfluenceAdapter_PlumbsAllFields(t *testing.T) {
	setEnv(t)
	setConfluenceEnv(t)
	t.Setenv("REPOMIND_CONFLUENCE_MAX_RETRIES", "5")
	t.Setenv("REPOMIND_CONFLUENCE_TIMEOUT", "12s")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	a := cfg.ConfluenceAdapter()
	if a.BaseURL != "https://acme.atlassian.net/wiki" ||
		a.Email != "ops@acme.com" ||
		a.APIToken != "tok" {
		t.Errorf("adapter creds = %+v", a)
	}
	if a.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5 (operator override)", a.MaxRetries)
	}
	if a.Timeout.String() != "12s" {
		t.Errorf("Timeout = %s, want 12s", a.Timeout)
	}
}

func TestConfluenceAdapter_DefaultMaxRetriesIsThree(t *testing.T) {
	setEnv(t)
	setConfluenceEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.ConfluenceAdapter().MaxRetries; got != 3 {
		t.Errorf("default MaxRetries = %d, want 3", got)
	}
}
