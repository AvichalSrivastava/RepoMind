package shared

import (
	"fmt"
	"regexp"
	"strings"
)

// ---------- Branch ----------

// Branch is a validated git branch reference. Domain code uses Branch
// instead of string for branch parameters to prevent accidental misuse.
type Branch struct {
	name string
}

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9_./\-]{1,255}$`)

// NewBranch validates and constructs a Branch.
func NewBranch(name string) (Branch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Branch{}, NewValidationError("branch", "must not be empty")
	}
	if !branchPattern.MatchString(name) {
		return Branch{}, NewValidationError("branch", fmt.Sprintf("invalid branch reference %q", name))
	}
	return Branch{name: name}, nil
}

// MustBranch is the panicking variant — use only with compile-time-known
// constants (typically in tests).
func MustBranch(name string) Branch {
	b, err := NewBranch(name)
	if err != nil {
		panic(err)
	}
	return b
}

// String returns the branch name.
func (b Branch) String() string { return b.name }

// IsZero reports whether the Branch was zero-valued (never constructed).
func (b Branch) IsZero() bool { return b.name == "" }

// Equal returns true if both branches name the same ref.
func (b Branch) Equal(other Branch) bool { return b.name == other.name }

// ---------- SHA ----------

// SHA is a validated git commit SHA (full or short form, 7..64 hex chars).
type SHA struct {
	value string
}

var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// NewSHA validates and constructs a SHA.
func NewSHA(s string) (SHA, error) {
	s = strings.TrimSpace(s)
	if !shaPattern.MatchString(s) {
		return SHA{}, NewValidationError("sha", fmt.Sprintf("invalid SHA %q", s))
	}
	return SHA{value: strings.ToLower(s)}, nil
}

// String returns the lower-cased hex SHA.
func (s SHA) String() string { return s.value }

// IsZero reports whether the SHA was zero-valued.
func (s SHA) IsZero() bool { return s.value == "" }

// ---------- Author ----------

// Author identifies a human actor associated with a commit/PR/review.
type Author struct {
	Login string // GitHub login
	Email string // optional
	Name  string // optional display name
}

// NewAuthor constructs an Author. Login is required.
func NewAuthor(login, name, email string) (Author, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return Author{}, NewValidationError("login", "must not be empty")
	}
	return Author{Login: login, Name: name, Email: email}, nil
}

// IsZero reports whether the Author was zero-valued.
func (a Author) IsZero() bool { return a.Login == "" }

// ---------- TicketKey ----------

// TicketKey is a validated external ticket identifier (e.g. Jira "PROJ-123").
type TicketKey struct {
	value string
}

var ticketKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}-[0-9]{1,10}$`)

// NewTicketKey validates and constructs a TicketKey.
func NewTicketKey(s string) (TicketKey, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if !ticketKeyPattern.MatchString(s) {
		return TicketKey{}, NewValidationError("ticket_key", fmt.Sprintf("invalid ticket key %q", s))
	}
	return TicketKey{value: s}, nil
}

// String returns the ticket key in canonical form.
func (t TicketKey) String() string { return t.value }

// IsZero reports whether the TicketKey was zero-valued.
func (t TicketKey) IsZero() bool { return t.value == "" }
