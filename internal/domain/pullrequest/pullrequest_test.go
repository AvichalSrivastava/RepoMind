package pullrequest_test

import (
	"errors"
	"testing"
	"time"

	"github.com/repomind/repomind/internal/domain/pullrequest"
	"github.com/repomind/repomind/internal/domain/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedClock(t *testing.T, iso string) shared.Clock {
	t.Helper()
	at, err := time.Parse(time.RFC3339, iso)
	require.NoError(t, err)
	return shared.FixedClock{T: at}
}

func validOpenInput(t *testing.T) pullrequest.OpenInput {
	t.Helper()
	author, err := shared.NewAuthor("alice", "Alice Example", "alice@example.com")
	require.NoError(t, err)
	return pullrequest.OpenInput{
		RepositoryID: shared.NewRepositoryID(),
		Number:       42,
		Title:        "Add login flow",
		Description:  "Implements OAuth login.",
		Author:       author,
		SourceBranch: shared.MustBranch("feature/login"),
		TargetBranch: shared.MustBranch("qa"),
		Labels:       []string{"feature", "feature", "auth"},
	}
}

func TestOpen_HappyPath(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	in := validOpenInput(t)

	pr, err := pullrequest.Open(in, clk)
	require.NoError(t, err)

	assert.Equal(t, pullrequest.StateOpen, pr.State())
	assert.Equal(t, pullrequest.Number(42), pr.Number())
	assert.Equal(t, "Add login flow", pr.Title())
	assert.ElementsMatch(t, []string{"feature", "auth"}, pr.Labels(), "labels deduplicated")
	assert.Equal(t, clk.Now(), pr.CreatedAt())

	events := pr.PullEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "pull_request.opened", events[0].EventName())
	assert.Empty(t, pr.PullEvents(), "PullEvents should drain")
}

func TestOpen_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(in *pullrequest.OpenInput)
	}{
		{"zero repo id", func(in *pullrequest.OpenInput) { in.RepositoryID = shared.RepositoryID{} }},
		{"non-positive number", func(in *pullrequest.OpenInput) { in.Number = 0 }},
		{"empty title", func(in *pullrequest.OpenInput) { in.Title = "  " }},
		{"zero author", func(in *pullrequest.OpenInput) { in.Author = shared.Author{} }},
		{"same source and target", func(in *pullrequest.OpenInput) { in.SourceBranch = in.TargetBranch }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			in := validOpenInput(t)
			tt.mutate(&in)
			_, err := pullrequest.Open(in, fixedClock(t, "2026-06-23T10:00:00Z"))
			require.Error(t, err)
		})
	}
}

func TestMerge_HappyPathAndIdempotency(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	pr, err := pullrequest.Open(validOpenInput(t), clk)
	require.NoError(t, err)
	pr.PullEvents() // drain open event

	sha, err := shared.NewSHA("deadbeefcafe1234567890abcdef0123456789ab")
	require.NoError(t, err)
	mergedAt := clk.Now().Add(time.Hour)

	require.NoError(t, pr.Merge(mergedAt, sha))
	assert.True(t, pr.IsMerged())
	require.NotNil(t, pr.MergedAt())
	assert.Equal(t, mergedAt, *pr.MergedAt())
	require.NotNil(t, pr.MergeSHA())
	assert.Equal(t, sha.String(), pr.MergeSHA().String())

	events := pr.PullEvents()
	require.Len(t, events, 1)
	assert.Equal(t, "pull_request.merged", events[0].EventName())

	// Idempotent merge with same SHA.
	require.NoError(t, pr.Merge(mergedAt, sha))
	assert.Empty(t, pr.PullEvents(), "no new event on idempotent merge")
}

func TestMerge_ConflictOnDifferentSHA(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	pr, _ := pullrequest.Open(validOpenInput(t), clk)
	sha1, _ := shared.NewSHA("deadbeefcafe1234567890abcdef0123456789ab")
	sha2, _ := shared.NewSHA("1234567890abcdef1234567890abcdef12345678")
	require.NoError(t, pr.Merge(clk.Now(), sha1))

	err := pr.Merge(clk.Now(), sha2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrConflict))
}

func TestClose_RejectsAfterMerge(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	pr, _ := pullrequest.Open(validOpenInput(t), clk)
	sha, _ := shared.NewSHA("deadbeefcafe1234567890abcdef0123456789ab")
	require.NoError(t, pr.Merge(clk.Now(), sha))

	err := pr.Close(clk.Now().Add(time.Hour))
	require.Error(t, err)
	assert.True(t, errors.Is(err, shared.ErrInvariant))
}

func TestAttachFile_DedupesByPath(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	pr, _ := pullrequest.Open(validOpenInput(t), clk)
	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path: "auth.go", Status: pullrequest.FileAdded, Additions: 10,
	}))
	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path: "auth.go", Status: pullrequest.FileModified, Additions: 25, Deletions: 3,
	}))
	require.NoError(t, pr.AttachFile(pullrequest.ChangedFile{
		Path: "auth_test.go", Status: pullrequest.FileAdded, Additions: 50,
	}))

	files := pr.Files()
	require.Len(t, files, 2)
	assert.Equal(t, "auth.go", files[0].Path)
	assert.Equal(t, pullrequest.FileModified, files[0].Status)
	assert.Equal(t, 75+3, pr.LinesChanged())
}

func TestRecordReview_UpdatesExisting(t *testing.T) {
	clk := fixedClock(t, "2026-06-23T10:00:00Z")
	pr, _ := pullrequest.Open(validOpenInput(t), clk)

	bob, _ := shared.NewAuthor("bob", "Bob", "bob@example.com")
	require.NoError(t, pr.RecordReview(pullrequest.Reviewer{
		Author: bob, State: pullrequest.ReviewRequested, At: clk.Now(),
	}))
	require.NoError(t, pr.RecordReview(pullrequest.Reviewer{
		Author: bob, State: pullrequest.ReviewApproved, At: clk.Now().Add(time.Hour),
	}))

	revs := pr.Reviewers()
	require.Len(t, revs, 1)
	assert.Equal(t, pullrequest.ReviewApproved, revs[0].State)
}
