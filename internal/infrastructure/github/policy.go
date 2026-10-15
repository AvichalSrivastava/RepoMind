// Package github (infrastructure layer) — repo target-branch policy.
//
// Policy lets operators express:
//   - "for every repo in REPOMIND_GITHUB_REPOS, only analyse PRs
//     whose base branch is one of these defaults"
//   - "but for these specific repos, use this custom list instead"
//
// Loaded from a YAML file at startup (path from
// REPOMIND_GITHUB_REPO_POLICY_PATH). When the env var is empty no
// policy is loaded and every PR -- regardless of base branch -- is
// accepted (current behaviour). This keeps the feature additive.
//
// File schema:
//
//	default_target_branches:
//	  - qa
//	repos:
//	  - full_name: SHL-India/TDM-data-management-ui
//	    target_branches:
//	      - test_sprint-183
//	  - full_name: SHL-India/TDM-tcplus-group-manager
//	    target_branches:
//	      - "release/sprint-*"
//
// Matching semantics:
//   - Repo full-name match: case-insensitive ("acme/foo" == "Acme/Foo").
//   - Branch match: case-insensitive ("Release/Sprint-187" ~ "release/sprint-187").
//   - Patterns containing `*` are treated as globs where `*` matches
//     any character sequence (including `/`).
//   - Empty target_branches for a repo entry means "explicitly mute"
//     -- every PR for that repo is filtered out.
//   - A repo not listed in `repos` falls through to
//     default_target_branches. If both are empty/absent the policy
//     accepts every branch for that repo (fail-open per-repo so
//     operators don't silently lose PRs after editing the allow-list).
package github

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RepoPolicy is the in-memory representation of repo-policy.yaml.
// A nil *RepoPolicy means "no policy configured" — all branches
// accepted everywhere. Use this distinction when wiring the
// dependency: pass nil to fall back to legacy behaviour.
type RepoPolicy struct {
	defaultBranches []string
	repos           map[string][]string // key: lower-cased full_name
	// Pre-compiled regex for any pattern containing '*'. Keyed by
	// the original (lower-cased) pattern so the matcher can fall
	// back to plain equality for non-glob patterns.
	globs map[string]*regexp.Regexp
}

// rawRepoPolicyFile is the on-disk yaml shape. Kept private so the
// public Go API stays stable across schema tweaks.
type rawRepoPolicyFile struct {
	DefaultTargetBranches []string             `yaml:"default_target_branches"`
	Repos                 []rawRepoPolicyEntry `yaml:"repos"`
}

type rawRepoPolicyEntry struct {
	FullName       string   `yaml:"full_name"`
	TargetBranches []string `yaml:"target_branches"`
}

// LoadRepoPolicyFromFile reads + parses + validates a policy file.
// Returns (nil, nil) when path is empty -- the caller treats that as
// "no policy configured". Returns an error for any malformed input
// so the operator gets a fail-fast at startup rather than silent
// permissive behaviour.
func LoadRepoPolicyFromFile(path string) (*RepoPolicy, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("github: load repo policy %q: %w", path, err)
	}

	var doc rawRepoPolicyFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("github: parse repo policy %q: %w", path, err)
	}

	return buildRepoPolicy(doc)
}

// buildRepoPolicy validates the raw document and normalises it into
// the lookup shape. Split out from the loader for direct testability
// without touching the filesystem.
func buildRepoPolicy(doc rawRepoPolicyFile) (*RepoPolicy, error) {
	p := &RepoPolicy{
		defaultBranches: cleanBranches(doc.DefaultTargetBranches),
		repos:           make(map[string][]string),
		globs:           make(map[string]*regexp.Regexp),
	}

	for i, entry := range doc.Repos {
		full := strings.TrimSpace(entry.FullName)
		if full == "" {
			return nil, fmt.Errorf("repo[%d]: full_name is required", i)
		}
		if !strings.Contains(full, "/") {
			return nil, fmt.Errorf("repo[%d]: full_name %q must be in owner/name form", i, full)
		}
		key := strings.ToLower(full)
		if _, dup := p.repos[key]; dup {
			return nil, fmt.Errorf("repo[%d]: duplicate entry for %q", i, full)
		}
		// Empty target_branches is LEGAL -- it means "mute this repo".
		// We persist the (possibly empty) cleaned slice so Accept can
		// distinguish "explicitly muted" from "not listed".
		p.repos[key] = cleanBranches(entry.TargetBranches)
	}

	// Pre-compile every glob pattern we know about so Accept stays
	// allocation-free on the hot path. Plain (non-glob) patterns use
	// equality matching and skip the regex table.
	for _, branches := range append([][]string{p.defaultBranches}, mapValues(p.repos)...) {
		for _, pattern := range branches {
			if !strings.ContainsRune(pattern, '*') {
				continue
			}
			lower := strings.ToLower(pattern)
			if _, exists := p.globs[lower]; exists {
				continue
			}
			re, err := compileGlob(lower)
			if err != nil {
				return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
			}
			p.globs[lower] = re
		}
	}

	return p, nil
}

// Accept returns true when a PR targeting `branch` in repo
// `owner/name` is in scope per the policy.
//
//   - nil *RepoPolicy        -> accept everything (legacy behaviour).
//   - branch == ""           -> accept (caller couldn't extract the
//     base branch; policy can't decide).
//   - repo overridden        -> match against repo's target_branches.
//   - repo not overridden    -> match against default_target_branches.
//   - both lists empty       -> accept (fail-open per-repo).
//
// Comparison is case-insensitive throughout.
func (p *RepoPolicy) Accept(owner, name, branch string) bool {
	if p == nil {
		return true
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return true
	}

	fullName := strings.ToLower(strings.TrimSpace(owner) + "/" + strings.TrimSpace(name))
	lowerBranch := strings.ToLower(branch)

	if patterns, ok := p.repos[fullName]; ok {
		// Repo override exists -- this is authoritative, including
		// the explicitly-empty "mute" case which must REJECT.
		return p.matchAny(patterns, lowerBranch)
	}

	// No override -- fall through to defaults.
	if len(p.defaultBranches) == 0 {
		// No defaults either; permissive fall-through prevents
		// silent loss after operator adds a repo to the allow-list
		// but forgets to set defaults.
		return true
	}
	return p.matchAny(p.defaultBranches, lowerBranch)
}

// matchAny reports whether lowerBranch matches any pattern. Plain
// patterns use equality; glob patterns use the pre-compiled regex.
func (p *RepoPolicy) matchAny(patterns []string, lowerBranch string) bool {
	for _, pat := range patterns {
		lowerPat := strings.ToLower(pat)
		if re, ok := p.globs[lowerPat]; ok {
			if re.MatchString(lowerBranch) {
				return true
			}
			continue
		}
		if lowerPat == lowerBranch {
			return true
		}
	}
	return false
}

// HasOverrideFor reports whether the policy contains an explicit
// repo entry. Useful for the composition root's startup log so
// operators can spot-check the parse result.
func (p *RepoPolicy) HasOverrideFor(owner, name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.repos[strings.ToLower(strings.TrimSpace(owner)+"/"+strings.TrimSpace(name))]
	return ok
}

// DefaultBranches returns a copy of the configured default targets.
// Empty if none configured. Returned by value so callers can't mutate
// the policy.
func (p *RepoPolicy) DefaultBranches() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.defaultBranches))
	copy(out, p.defaultBranches)
	return out
}

// OverrideCount returns how many repos have explicit overrides.
func (p *RepoPolicy) OverrideCount() int {
	if p == nil {
		return 0
	}
	return len(p.repos)
}

// ---------- helpers ----------

// cleanBranches trims whitespace and drops empty entries. The
// resulting slice may be empty (caller may treat that as "mute").
func cleanBranches(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// mapValues collects the values of m in undefined order. Sole use is
// pre-compiling globs at startup; ordering does not matter.
func mapValues(m map[string][]string) [][]string {
	out := make([][]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// compileGlob converts a `*`-glob into a regexp. `*` matches any
// character sequence including `/` (intentional: we want
// `release/sprint-*` to match `release/sprint-187` AND
// `release/sprint/extra/187` if it ever appears -- glob-style not
// shell-style). Everything else is regex-escaped.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, errors.New("empty pattern")
	}
	var b strings.Builder
	b.Grow(len(pattern) + 4)
	b.WriteByte('^')
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '.', '+', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
