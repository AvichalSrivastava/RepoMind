package github

import (
	gh "github.com/google/go-github/v63/github"

	apigithub "github.com/repomind/repomind/internal/application/github"
)

// toPortPR maps a go-github *PullRequest to the port DTO. nil-safe on
// nested optional fields.
func toPortPR(src *gh.PullRequest) apigithub.PullRequest {
	if src == nil {
		return apigithub.PullRequest{}
	}

	out := apigithub.PullRequest{
		Number:       src.GetNumber(),
		Title:        src.GetTitle(),
		Body:         src.GetBody(),
		State:        apigithub.PRState(src.GetState()),
		Merged:       src.GetMerged() || src.MergedAt != nil,
		BaseBranch:   src.GetBase().GetRef(),
		HeadBranch:   src.GetHead().GetRef(),
		MergeSHA:     src.GetMergeCommitSHA(),
		Author:       userRef(src.GetUser()),
		Additions:    src.GetAdditions(),
		Deletions:    src.GetDeletions(),
		ChangedFiles: src.GetChangedFiles(),
		URL:          src.GetHTMLURL(),
	}
	if t := src.CreatedAt; t != nil {
		out.CreatedAt = t.Time
	}
	if t := src.UpdatedAt; t != nil {
		out.UpdatedAt = t.Time
	}
	if t := src.MergedAt; t != nil {
		v := t.Time
		out.MergedAt = &v
	}
	if t := src.ClosedAt; t != nil {
		v := t.Time
		out.ClosedAt = &v
	}
	for _, l := range src.Labels {
		if l != nil && l.Name != nil {
			out.Labels = append(out.Labels, *l.Name)
		}
	}
	// When MergeSHA is unset on a merged PR (rare), clear Merged so the
	// caller doesn't trust it.
	if out.Merged && out.MergeSHA == "" && out.MergedAt == nil {
		out.Merged = false
	}
	return out
}

func toPortFile(src *gh.CommitFile) apigithub.File {
	if src == nil {
		return apigithub.File{}
	}
	return apigithub.File{
		Path:         src.GetFilename(),
		Status:       src.GetStatus(),
		Additions:    src.GetAdditions(),
		Deletions:    src.GetDeletions(),
		Changes:      src.GetChanges(),
		PreviousPath: src.GetPreviousFilename(),
	}
}

func toPortCommit(src *gh.RepositoryCommit) apigithub.Commit {
	if src == nil {
		return apigithub.Commit{}
	}
	out := apigithub.Commit{
		SHA:       src.GetSHA(),
		Message:   src.GetCommit().GetMessage(),
		Author:    userRef(src.GetAuthor()),
		Committer: userRef(src.GetCommitter()),
	}
	// Prefer the committer date (when the commit landed); fall back to
	// the author date.
	if c := src.GetCommit(); c != nil {
		if d := c.GetCommitter().GetDate(); !d.IsZero() {
			out.Timestamp = d.Time
		} else if d := c.GetAuthor().GetDate(); !d.IsZero() {
			out.Timestamp = d.Time
		}
	}
	return out
}

func toPortReview(src *gh.PullRequestReview) apigithub.Review {
	if src == nil {
		return apigithub.Review{}
	}
	out := apigithub.Review{
		ID:       src.GetID(),
		Reviewer: userRef(src.GetUser()),
		State:    src.GetState(),
		Body:     src.GetBody(),
	}
	if t := src.SubmittedAt; t != nil {
		out.SubmittedAt = t.Time
	}
	return out
}

func userRef(u *gh.User) apigithub.UserRef {
	if u == nil {
		return apigithub.UserRef{}
	}
	return apigithub.UserRef{
		Login: u.GetLogin(),
		Name:  u.GetName(),
	}
}
