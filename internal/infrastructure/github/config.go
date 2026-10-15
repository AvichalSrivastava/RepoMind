package github

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config holds GitHub adapter settings. All fields except BaseURL are
// required. The adapter authenticates as a GitHub App installation.
type Config struct {
	// AppID is the numeric GitHub App ID assigned at app creation.
	AppID int64

	// InstallationID is the per-account install id (the last number in
	// the URL after the App is installed).
	InstallationID int64

	// PrivateKeyPath points at the PEM file downloaded when the App's
	// private key was generated.
	PrivateKeyPath string

	// BaseURL overrides https://api.github.com (set for GitHub
	// Enterprise Server, e.g. https://github.shl.com/api/v3). Empty
	// means github.com.
	BaseURL string

	// WebhookSecret is the HMAC secret used to verify incoming webhook
	// deliveries. Required when the HTTP webhook handler is wired in.
	WebhookSecret string

	// Timeout caps a single HTTP call.
	Timeout time.Duration

	// MaxRetries bounds transient-failure retries (429 / 5xx).
	MaxRetries int

	// Repos is the closed list of owner/name pairs RepoMind is allowed
	// to query. Anything else returns ErrUnauthorizedRepo at the
	// adapter boundary even if the App could technically read it.
	Repos []RepoSpec
}

// RepoSpec is a configured owner/name pair.
type RepoSpec struct {
	Owner string
	Name  string
}

// String renders as "owner/name".
func (r RepoSpec) String() string { return r.Owner + "/" + r.Name }

// ParseRepoList splits a comma-separated "owner/name,owner/name,..."
// list (as supplied via REPOMIND_GITHUB_REPOS) into RepoSpecs and
// returns an error if any entry is malformed.
func ParseRepoList(csv string) ([]RepoSpec, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, errors.New("github: repo list is empty")
	}
	parts := strings.Split(csv, ",")
	out := make([]RepoSpec, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		slash := strings.IndexByte(p, '/')
		if slash <= 0 || slash == len(p)-1 {
			return nil, fmt.Errorf("github: invalid repo spec %q (want owner/name)", p)
		}
		owner := strings.TrimSpace(p[:slash])
		name := strings.TrimSpace(p[slash+1:])
		if owner == "" || name == "" {
			return nil, fmt.Errorf("github: invalid repo spec %q", p)
		}
		key := strings.ToLower(owner + "/" + name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, RepoSpec{Owner: owner, Name: name})
	}
	if len(out) == 0 {
		return nil, errors.New("github: repo list is empty after parsing")
	}
	return out, nil
}

// validate enforces invariants. Called by NewClient.
func (c *Config) validate() error {
	if c.AppID <= 0 {
		return fmt.Errorf("github: app id must be > 0, got %d", c.AppID)
	}
	if c.InstallationID <= 0 {
		return fmt.Errorf("github: installation id must be > 0, got %d", c.InstallationID)
	}
	if strings.TrimSpace(c.PrivateKeyPath) == "" {
		return errors.New("github: private key path is empty")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Host == "" || !strings.HasPrefix(u.Scheme, "http") {
			return fmt.Errorf("github: invalid base url %q", c.BaseURL)
		}
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("github: max_retries must be >= 0, got %d", c.MaxRetries)
	}
	if c.Timeout < 0 {
		return fmt.Errorf("github: timeout must be >= 0")
	}
	if len(c.Repos) == 0 {
		return errors.New("github: at least one repo must be configured")
	}
	return nil
}
