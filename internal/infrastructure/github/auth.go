package github

import (
	"net/http"
	"time"

	ghi "github.com/bradleyfalzon/ghinstallation/v2"
)

// newInstallationHTTPClient builds an *http.Client whose Transport
// authenticates as the GitHub App installation. Tokens are minted from
// the App's private key (JWT) and cached/renewed automatically by
// ghinstallation/v2 — we do not manage their lifetime.
//
// We keep this in its own file so callers (and tests) can swap it for
// a fake transport without touching the REST client.
func newInstallationHTTPClient(cfg Config) (*http.Client, error) {
	base := http.DefaultTransport
	tr, err := ghi.NewKeyFromFile(base, cfg.AppID, cfg.InstallationID, cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	if cfg.BaseURL != "" {
		// ghinstallation uses this for the App API itself (to mint
		// installation tokens). For GHES the API base is the same as
		// for the REST client.
		tr.BaseURL = cfg.BaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
