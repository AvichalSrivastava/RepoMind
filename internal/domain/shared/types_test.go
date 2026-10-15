package shared_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBranch(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"simple", "main", false},
		{"slashes", "feature/abc-123", false},
		{"dots and dashes", "release/1.2.3-rc", false},
		{"empty rejected", "", true},
		{"whitespace rejected", "   ", true},
		{"illegal char rejected", "feature with spaces", true},
		{"too long rejected", string(make([]byte, 300)), true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			b, err := shared.NewBranch(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, shared.ErrInvalidArgument), "expected ErrInvalidArgument")
				assert.True(t, b.IsZero())
				return
			}
			require.NoError(t, err)
			assert.False(t, b.IsZero())
		})
	}
}

func TestNewSHA(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"abcdef0", false},
		{"ABCDEF0123456789abcdef0123456789ABCDEF01", false},
		{"too-short", true},
		{"zzz", true},
		{"", true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.in, func(t *testing.T) {
			s, err := shared.NewSHA(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, s.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, strings.ToLower(tt.in), s.String(), "should be lowercased: %q", s.String())
		})
	}
}

func TestNewTicketKey(t *testing.T) {
	good, err := shared.NewTicketKey("proj-123")
	require.NoError(t, err)
	assert.Equal(t, "PROJ-123", good.String(), "should canonicalize to upper")

	_, err = shared.NewTicketKey("not-a-key")
	require.Error(t, err)
}

func TestIDTypeSafety(t *testing.T) {
	// This test exists to document the intent: distinct ID types are
	// not assignable across aggregates. The proof is at compile time —
	// uncommenting the line below would fail to compile, which is the
	// guarantee we want.
	//
	// var x shared.RepositoryID = shared.NewPullRequestID()
	repoID := shared.NewRepositoryID()
	prID := shared.NewPullRequestID()
	assert.NotEqual(t, repoID.String(), prID.String())
}
