package github

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// LoadRepoPolicyFromBytesForTest is a test-only entry point that
// bypasses file I/O. Only compiled when the package is built under
// `go test`. Exported (capital L) so external test packages can use
// it, but only because the _test.go suffix limits visibility to
// tests.
func LoadRepoPolicyFromBytesForTest(_ *testing.T, body string) (*RepoPolicy, error) {
	var doc rawRepoPolicyFile
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, err
	}
	return buildRepoPolicy(doc)
}
