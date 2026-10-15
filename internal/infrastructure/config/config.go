// Package config loads RepoMind configuration from environment variables
// (12-factor) with sane production defaults.
//
// Env vars are prefixed REPOMIND_*. See configs/config.example.env for
// the full schema.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"

	"github.com/repomind/repomind/internal/application/llm"
	infraconfluence "github.com/repomind/repomind/internal/infrastructure/confluence"
	infragithub "github.com/repomind/repomind/internal/infrastructure/github"
	infrajira "github.com/repomind/repomind/internal/infrastructure/jira"
	infraopenai "github.com/repomind/repomind/internal/infrastructure/openai"
)

// Config is the root configuration aggregate.
type Config struct {
	HTTP     HTTP     `split_words:"true"`
	Log      Log      `split_words:"true"`
	OTEL     OTEL     `split_words:"true"`
	Metrics  Metrics  `split_words:"true"`
	Postgres Postgres `split_words:"true"`
	// GitHub uses an explicit envconfig tag to keep the prefix
	// REPOMIND_GITHUB_* instead of envconfig's default split of
	// "GitHub" into "GIT_HUB" (which would yield REPOMIND_GIT_HUB_*).
	GitHub     GitHub     `envconfig:"GITHUB"`
	Jira       Jira       `split_words:"true"`
	Confluence Confluence `split_words:"true"`
	AI         AI         `split_words:"true"`
	Worker     Worker     `split_words:"true"`
	Ticker     Ticker     `split_words:"true"`
}

// HTTP holds HTTP server settings.
//
// APIKey is the shared secret required on operator endpoints
// (`/api/v1/*`) via the Authorization: Bearer header. When unset, the
// operator surface is *not registered at all* — the webhook surface
// and `/healthz` remain available. This degrades safely on dev boxes
// without exposing privileged routes unauthenticated.
type HTTP struct {
	Addr            string        `default:":8080"`
	ReadTimeout     time.Duration `split_words:"true" default:"15s"`
	WriteTimeout    time.Duration `split_words:"true" default:"30s"`
	ShutdownTimeout time.Duration `split_words:"true" default:"15s"`
	APIKey          string        `split_words:"true"`
}

// Log controls structured logging.
type Log struct {
	Level  string `default:"info"` // debug | info | warn | error
	Format string `default:"json"` // json | console
}

// OTEL controls OpenTelemetry tracing.
type OTEL struct {
	ExporterOTLPEndpoint string `envconfig:"EXPORTER_OTLP_ENDPOINT"`
}

// Metrics controls Prometheus exposition.
type Metrics struct {
	Addr string `default:":9090"`
}

// Postgres holds database connection settings.
type Postgres struct {
	DSN      string `required:"true"`
	MaxConns int32  `split_words:"true" default:"20"`
	MinConns int32  `split_words:"true" default:"2"`
}

// GitHub holds GitHub App authentication settings.
type GitHub struct {
	AppID          int64         `split_words:"true"`
	InstallationID int64         `split_words:"true"`
	PrivateKeyPath string        `split_words:"true"`
	BaseURL        string        `split_words:"true"` // empty = github.com
	WebhookSecret  string        `split_words:"true"`
	Timeout        time.Duration `default:"30s"`
	MaxRetries     int           `split_words:"true" default:"3"`

	// Repos is a comma-separated list of "owner/name" pairs the
	// adapter is allowed to query. Parsed via
	// infragithub.ParseRepoList into a []RepoSpec at the composition
	// root.
	Repos string `envconfig:"REPOS"`

	// RepoPolicyPath is the (optional) path to a YAML file expressing
	// per-repo PR target-branch filters. When empty, no branch
	// filtering happens and every PR in REPOS is analysed. See
	// configs/repo-policy.example.yaml for the schema.
	RepoPolicyPath string `envconfig:"REPO_POLICY_PATH"`
}

// Jira holds Jira REST API settings. Jira is the sprint source of truth.
type Jira struct {
	BaseURL    string        `split_words:"true" required:"true"`
	AuthMode   string        `split_words:"true" default:"basic"` // basic | bearer
	Email      string        `split_words:"true"`                 // required when auth_mode=basic
	APIToken   string        `split_words:"true" required:"true"`
	Timeout    time.Duration `default:"30s"`
	MaxRetries int           `split_words:"true" default:"3"`

	// ProjectKey is the Jira project we filter by (e.g. "SIS").
	ProjectKey string `split_words:"true" required:"true"`

	// BoardID is the agile board id (numeric in the Jira UI URL).
	BoardID int `split_words:"true" required:"true"`

	// TeamName identifies which sub-team on the shared board the
	// release notes are for (e.g. "Marvelous"). V2 will turn this
	// scalar into a list — see docs/ARCHITECTURE.md §6.
	TeamName string `split_words:"true" required:"true"`

	// SprintNamePrefix filters sprints on shared boards. Empty
	// defaults to "<TeamName>_Sprint_".
	SprintNamePrefix string `split_words:"true"`

	// QAOwnerFieldID is the Jira custom-field id that carries the
	// "QA Owner" user picker (e.g. "customfield_10173"). When set,
	// ListSprintIssues requests this field and decodes it as the
	// QA Owner display name. Empty leaves QAOwner blank -- the
	// release-notes handler then falls back to the current
	// assignee for the QA Owner column (workflow-dependent).
	QAOwnerFieldID string `split_words:"true"`
}

// Confluence holds Confluence REST API settings.
//
// Two modes are supported:
//
//   - Single-page (legacy): PageID set, ParentID empty. RepoMind
//     updates one pre-configured page for every sprint.
//   - Per-sprint (default going forward): ParentID + SpaceKey set,
//     PageID may be empty. RepoMind creates one child page per
//     sprint under ParentID (which may be a folder or a page) and
//     tracks (sprint_id -> page_id) in the sprint_confluence_page
//     table.
type Confluence struct {
	BaseURL    string        `split_words:"true"`
	Email      string        `split_words:"true"`
	APIToken   string        `split_words:"true"`
	PageID     string        `split_words:"true"`
	SpaceKey   string        `split_words:"true"` // required in per-sprint mode; e.g. "SHL360"
	ParentID   string        `split_words:"true"` // parent page or folder id; when set enables per-sprint mode
	Timeout    time.Duration `default:"30s"`
	MaxRetries int           `split_words:"true" default:"3"` // 429 / 5xx (client) and 409 (publisher) retry budget
}

// AI holds OpenAI-compatible provider settings. The same protocol is
// spoken by OpenAI, Azure OpenAI, and GitHub Models, selected via
// Deployment.
type AI struct {
	APIKey     string        `split_words:"true" required:"true"`
	Model      string        `default:"gpt-4o-mini"`
	BaseURL    string        `split_words:"true"`
	Timeout    time.Duration `default:"60s"`
	MaxRetries int           `split_words:"true" default:"3"`

	// Deployment selects deployment-specific defaults and auth
	// header style: openai | github_models | azure_openai.
	Deployment string `default:"openai"`

	// AzureAPIVersion is appended as ?api-version=... when
	// Deployment=azure_openai. Ignored otherwise.
	AzureAPIVersion string `split_words:"true"`

	// Per-role model overrides. Empty means "use Model". Useful for
	// running release notes on a flagship model while keeping risk /
	// QA on a cheap fast model.
	RiskModel         string `split_words:"true"`
	ReleaseNotesModel string `split_words:"true"`
	QAModel           string `split_words:"true"`
}

// Worker controls the M10 outbox dispatcher: how often it polls the
// outbox as a fallback to LISTEN/NOTIFY, how large each drain batch
// is, how many times a failed event is retried before being
// permanently failed, and the backoff curve between retries.
//
// Defaults are tuned for low-volume V1 deployments. Raise BatchSize
// + concurrency once we have metrics; for now single-threaded serial
// processing keeps debugging straightforward.
type Worker struct {
	// PollInterval is the LISTEN/NOTIFY safety net. Even with a
	// working notification channel, the dispatcher drains on this
	// cadence to catch (a) notifications missed during reconnect
	// and (b) events whose next_attempt_at has just become due.
	PollInterval time.Duration `split_words:"true" default:"30s"`

	// BatchSize caps how many pending events ListPending returns
	// per call. Drain loops back until a partial batch arrives.
	BatchSize int `split_words:"true" default:"32"`

	// MaxAttempts is the total number of times a single event is
	// retried before MarkPermanentlyFailed. Attempts counter starts
	// at 0; the 5th failure flips status to 'failed'.
	MaxAttempts int `split_words:"true" default:"5"`

	// BaseBackoff is the first retry delay. Subsequent retries
	// double it (exponential), capped at 1h internally.
	BaseBackoff time.Duration `split_words:"true" default:"30s"`

	// HandlerTimeout caps how long a single handler invocation can
	// take before the dispatcher cancels its context. The handler
	// MUST honour ctx.Done(). LLM-bound handlers (release notes)
	// often need a higher per-handler override -- raise this only
	// if 2m is genuinely tight.
	HandlerTimeout time.Duration `split_words:"true" default:"2m"`

	// ShutdownGrace is the time the dispatcher waits for in-flight
	// handlers to finish after ctx cancellation. Past this, it logs
	// a warning and returns -- the partial-work outbox row will
	// remain at status='pending' and be retried on next start.
	ShutdownGrace time.Duration `split_words:"true" default:"30s"`
}

// Ticker controls the M10 sprint-state poller: a goroutine that
// queries Jira every Interval, compares the active sprint to the
// worker_state bookmark, and enqueues a release_notes.requested
// event when state transitions are detected.
type Ticker struct {
	// Enabled lets operators disable the ticker entirely (e.g.
	// during a Jira outage, or when running an operator-only
	// instance that should only respond to manual REST triggers).
	Enabled bool `default:"true"`

	// Interval between ticker passes. Jira sprint state changes
	// rarely, so 5m is generous; under that adds load to Jira
	// without value.
	Interval time.Duration `default:"5m"`
}

// Load reads configuration from the environment, applies defaults, and
// validates invariants. It returns an error if any required value is
// missing or invalid.
func Load() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("REPOMIND", &cfg); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: invalid: %w", err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "console":
	default:
		return fmt.Errorf("log.format must be json or console, got %q", c.Log.Format)
	}
	if c.Postgres.MinConns < 0 || c.Postgres.MaxConns < c.Postgres.MinConns {
		return fmt.Errorf("postgres.max_conns must be >= min_conns >= 0")
	}

	// HTTP validation. APIKey is optional (degrades to "operator
	// surface disabled"); the timeouts must be sane positives.
	if c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 || c.HTTP.ShutdownTimeout <= 0 {
		return fmt.Errorf("http.*_timeout must be > 0")
	}
	if c.HTTP.APIKey != "" && len(c.HTTP.APIKey) < 16 {
		return fmt.Errorf("http.api_key must be >= 16 chars when set (got %d)", len(c.HTTP.APIKey))
	}

	// Jira validation.
	if u, err := url.Parse(c.Jira.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("jira.base_url must be a valid http(s) URL, got %q", c.Jira.BaseURL)
	}
	switch c.Jira.AuthMode {
	case "basic":
		if c.Jira.Email == "" {
			return fmt.Errorf("jira.email is required when jira.auth_mode=basic")
		}
	case "bearer":
		// nothing extra — token already required.
	default:
		return fmt.Errorf("jira.auth_mode must be basic or bearer, got %q", c.Jira.AuthMode)
	}
	if strings.TrimSpace(c.Jira.ProjectKey) == "" {
		return fmt.Errorf("jira.project_key is required")
	}
	if c.Jira.BoardID <= 0 {
		return fmt.Errorf("jira.board_id must be > 0, got %d", c.Jira.BoardID)
	}
	if strings.TrimSpace(c.Jira.TeamName) == "" {
		return fmt.Errorf("jira.team_name is required")
	}

	// GitHub validation. The repo list is required even when the App
	// isn't installed yet, because it's our allow-list and missing it
	// would fail-open.
	if strings.TrimSpace(c.GitHub.Repos) == "" {
		return fmt.Errorf("github.repos is required (comma-separated owner/name list)")
	}
	if _, err := infragithub.ParseRepoList(c.GitHub.Repos); err != nil {
		return fmt.Errorf("github.repos: %w", err)
	}
	if c.GitHub.MaxRetries < 0 {
		return fmt.Errorf("github.max_retries must be >= 0, got %d", c.GitHub.MaxRetries)
	}
	if c.GitHub.Timeout < 0 {
		return fmt.Errorf("github.timeout must be >= 0")
	}

	// Confluence is optional but if any field is set, the set must be coherent.
	confSet := c.Confluence.BaseURL != "" || c.Confluence.PageID != "" ||
		c.Confluence.Email != "" || c.Confluence.APIToken != "" ||
		c.Confluence.ParentID != "" || c.Confluence.SpaceKey != ""
	if confSet {
		if c.Confluence.BaseURL == "" || c.Confluence.Email == "" || c.Confluence.APIToken == "" {
			return fmt.Errorf("confluence: base_url, email, and api_token must all be set together")
		}
		perSprint := c.Confluence.ParentID != ""
		if perSprint {
			if c.Confluence.SpaceKey == "" {
				return fmt.Errorf("confluence.space_key is required when parent_id is set (per-sprint mode)")
			}
		} else {
			if c.Confluence.PageID == "" {
				return fmt.Errorf("confluence.page_id is required when parent_id is not set (single-page mode)")
			}
		}
		if u, err := url.Parse(c.Confluence.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("confluence.base_url must be a valid http(s) URL, got %q", c.Confluence.BaseURL)
		}
		if c.Confluence.MaxRetries < 0 {
			return fmt.Errorf("confluence.max_retries must be >= 0, got %d", c.Confluence.MaxRetries)
		}
		if c.Confluence.Timeout < 0 {
			return fmt.Errorf("confluence.timeout must be >= 0")
		}
	}

	// AI / LLM validation.
	switch c.AI.Deployment {
	case "openai", "github_models", "azure_openai":
	default:
		return fmt.Errorf("ai.deployment must be one of openai|github_models|azure_openai, got %q", c.AI.Deployment)
	}
	if c.AI.MaxRetries < 0 {
		return fmt.Errorf("ai.max_retries must be >= 0, got %d", c.AI.MaxRetries)
	}
	if c.AI.Timeout < 0 {
		return fmt.Errorf("ai.timeout must be >= 0")
	}
	if b := strings.TrimSpace(c.AI.BaseURL); b != "" {
		if u, err := url.Parse(b); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("ai.base_url must be a valid http(s) URL, got %q", c.AI.BaseURL)
		}
	}

	// Worker validation. All durations must be positive; batch +
	// retry counts must be sane.
	if c.Worker.PollInterval <= 0 {
		return fmt.Errorf("worker.poll_interval must be > 0")
	}
	if c.Worker.BatchSize <= 0 || c.Worker.BatchSize > 1000 {
		return fmt.Errorf("worker.batch_size must be in (0, 1000], got %d", c.Worker.BatchSize)
	}
	if c.Worker.MaxAttempts <= 0 || c.Worker.MaxAttempts > 100 {
		return fmt.Errorf("worker.max_attempts must be in (0, 100], got %d", c.Worker.MaxAttempts)
	}
	if c.Worker.BaseBackoff <= 0 {
		return fmt.Errorf("worker.base_backoff must be > 0")
	}
	if c.Worker.HandlerTimeout <= 0 {
		return fmt.Errorf("worker.handler_timeout must be > 0")
	}
	if c.Worker.ShutdownGrace <= 0 {
		return fmt.Errorf("worker.shutdown_grace must be > 0")
	}

	// Ticker validation -- only meaningful when enabled.
	if c.Ticker.Enabled && c.Ticker.Interval <= 0 {
		return fmt.Errorf("ticker.interval must be > 0 when ticker.enabled=true")
	}

	return nil
}

// ResolvedSprintNamePrefix returns the configured prefix, defaulting
// to "<TeamName>_Sprint_" when SprintNamePrefix is unset.
func (j Jira) ResolvedSprintNamePrefix() string {
	if s := strings.TrimSpace(j.SprintNamePrefix); s != "" {
		return s
	}
	return j.TeamName + "_Sprint_"
}

// JiraAdapter projects this config into the value the Jira adapter
// expects. It does not perform I/O — pure data shaping.
func (c *Config) JiraAdapter() infrajira.Config {
	return infrajira.Config{
		BaseURL:        c.Jira.BaseURL,
		AuthMode:       infrajira.AuthMode(c.Jira.AuthMode),
		Email:          c.Jira.Email,
		APIToken:       c.Jira.APIToken,
		Timeout:        c.Jira.Timeout,
		MaxRetries:     c.Jira.MaxRetries,
		QAOwnerFieldID: c.Jira.QAOwnerFieldID,
	}
}

// GitHubAdapter projects this config into the value the GitHub adapter
// expects. It parses Repos (CSV) into []RepoSpec; callers MUST call
// Load() first (which already validates the list shape), so this can
// only fail if the input was mutated after Load.
func (c *Config) GitHubAdapter() (infragithub.Config, error) {
	repos, err := infragithub.ParseRepoList(c.GitHub.Repos)
	if err != nil {
		return infragithub.Config{}, fmt.Errorf("config: github.repos: %w", err)
	}
	return infragithub.Config{
		AppID:          c.GitHub.AppID,
		InstallationID: c.GitHub.InstallationID,
		PrivateKeyPath: c.GitHub.PrivateKeyPath,
		BaseURL:        c.GitHub.BaseURL,
		WebhookSecret:  c.GitHub.WebhookSecret,
		Timeout:        c.GitHub.Timeout,
		MaxRetries:     c.GitHub.MaxRetries,
		Repos:          repos,
	}, nil
}

// LoadRepoPolicy reads + validates the repo-policy YAML file. Returns
// (nil, nil) when RepoPolicyPath is empty -- callers treat that as
// "no branch filtering configured" and the webhook handler accepts
// every PR (legacy behaviour, additive feature).
//
// Lives on Config (not on the GitHub struct directly) so the composition
// root has a single hub for "load anything that needs I/O".
func (c *Config) LoadRepoPolicy() (*infragithub.RepoPolicy, error) {
	return infragithub.LoadRepoPolicyFromFile(c.GitHub.RepoPolicyPath)
}

// OpenAIAdapter projects this config into the value the OpenAI-compatible
// LLM adapter expects. It applies deployment-aware BaseURL defaults so
// that the dev fast-path (GitHub Models) works with just an API key.
func (c *Config) OpenAIAdapter() infraopenai.Config {
	baseURL := strings.TrimSpace(c.AI.BaseURL)
	deployment := infraopenai.Deployment(c.AI.Deployment)
	if baseURL == "" {
		switch deployment {
		case infraopenai.DeploymentGitHubModels:
			baseURL = "https://models.github.ai/inference"
		case infraopenai.DeploymentOpenAI, "":
			baseURL = "https://api.openai.com/v1"
		}
		// Azure has no sensible default — operator must set BaseURL.
	}

	modelByRole := map[llm.ModelRole]string{}
	if s := strings.TrimSpace(c.AI.RiskModel); s != "" {
		modelByRole[llm.RoleRisk] = s
	}
	if s := strings.TrimSpace(c.AI.ReleaseNotesModel); s != "" {
		modelByRole[llm.RoleReleaseNotes] = s
	}
	if s := strings.TrimSpace(c.AI.QAModel); s != "" {
		modelByRole[llm.RoleQA] = s
	}

	return infraopenai.Config{
		Deployment:      deployment,
		BaseURL:         baseURL,
		APIKey:          c.AI.APIKey,
		AzureAPIVersion: c.AI.AzureAPIVersion,
		DefaultModel:    c.AI.Model,
		ModelByRole:     modelByRole,
		Timeout:         c.AI.Timeout,
		MaxRetries:      c.AI.MaxRetries,
	}
}

// HasConfluence reports whether the operator has configured a
// Confluence target. The publisher is optional in V1 (dev runs can
// skip it and still exercise the analyzers end-to-end), so the
// composition root must consult this before constructing the adapter.
func (c *Config) HasConfluence() bool {
	return c.Confluence.BaseURL != "" && c.Confluence.PageID != "" &&
		c.Confluence.Email != "" && c.Confluence.APIToken != ""
}

// ConfluenceAdapter projects this config into the value the
// Confluence adapter expects. Callers must call HasConfluence first;
// passing an empty config returns a value that fails NewClient
// validation, by design.
func (c *Config) ConfluenceAdapter() infraconfluence.Config {
	return infraconfluence.Config{
		BaseURL:    c.Confluence.BaseURL,
		Email:      c.Confluence.Email,
		APIToken:   c.Confluence.APIToken,
		Timeout:    c.Confluence.Timeout,
		MaxRetries: c.Confluence.MaxRetries,
	}
}
