package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/repomind/repomind/internal/application/persistence"
	"github.com/repomind/repomind/internal/domain/analysis"
	"github.com/repomind/repomind/internal/domain/shared"
)

type analysesRepo struct{ q dbtx }

var _ persistence.Analyses = (*analysesRepo)(nil)

const analysisSelectCols = `
	id, kind,
	subject_pr_id, subject_sprint_id, subject_release_id,
	analyzer_name, analyzer_version, prompt_version,
	payload, model, tokens_used, generated_at
`

func (r *analysesRepo) Save(ctx context.Context, a *analysis.AnalysisResult) error {
	payloadJSON, err := analysis.MarshalPayload(a.Payload())
	if err != nil {
		return fmt.Errorf("postgres save analysis: marshal payload: %w", err)
	}

	subj := a.Subject()
	var prID, sprintID, releaseID *uuid.UUID
	if subj.PullRequestID != nil {
		u := subj.PullRequestID.UUID()
		prID = &u
	}
	if subj.SprintID != nil {
		u := subj.SprintID.UUID()
		sprintID = &u
	}
	if subj.ReleaseID != nil {
		u := subj.ReleaseID.UUID()
		releaseID = &u
	}

	// Analyses are append-only: each agent run produces a new row.
	// We deliberately omit ON CONFLICT — duplicate IDs are a bug, not a
	// legitimate upsert, and should surface as ErrConflict.
	const q = `
INSERT INTO analysis (
	id, kind,
	subject_pr_id, subject_sprint_id, subject_release_id,
	analyzer_name, analyzer_version, prompt_version,
	payload, model, tokens_used, generated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	_, err = r.q.Exec(ctx, q,
		a.ID().UUID(),
		string(a.Kind()),
		prID, sprintID, releaseID,
		a.Analyzer().Name,
		a.Analyzer().Version,
		a.Analyzer().PromptVersion,
		payloadJSON,
		a.Model(),
		a.TokensUsed(),
		a.GeneratedAt(),
	)
	return classify("save analysis", err)
}

func (r *analysesRepo) FindByID(ctx context.Context, id shared.AnalysisID) (*analysis.AnalysisResult, error) {
	return r.findOne(ctx, "WHERE id = $1", id.UUID())
}

func (r *analysesRepo) ListByPullRequest(
	ctx context.Context,
	prID shared.PullRequestID,
	kind analysis.Kind,
) ([]*analysis.AnalysisResult, error) {
	if kind == "" {
		return r.list(ctx,
			"WHERE subject_pr_id = $1 ORDER BY generated_at DESC",
			prID.UUID())
	}
	return r.list(ctx,
		"WHERE subject_pr_id = $1 AND kind = $2 ORDER BY generated_at DESC",
		prID.UUID(), string(kind))
}

func (r *analysesRepo) ListByRelease(
	ctx context.Context,
	relID shared.ReleaseID,
	kind analysis.Kind,
) ([]*analysis.AnalysisResult, error) {
	if kind == "" {
		return r.list(ctx,
			"WHERE subject_release_id = $1 ORDER BY generated_at DESC",
			relID.UUID())
	}
	return r.list(ctx,
		"WHERE subject_release_id = $1 AND kind = $2 ORDER BY generated_at DESC",
		relID.UUID(), string(kind))
}

func (r *analysesRepo) findOne(ctx context.Context, where string, args ...any) (*analysis.AnalysisResult, error) {
	rows, err := r.q.Query(ctx, "SELECT "+analysisSelectCols+" FROM analysis "+where+" LIMIT 1", args...)
	if err != nil {
		return nil, classify("find analysis", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, classify("find analysis: rows", err)
		}
		// Must pass pgx.ErrNoRows so classify maps to
		// persistence.ErrNotFound (see pullrequests.go comment).
		return nil, classify("find analysis", pgx.ErrNoRows)
	}
	return scanAnalysis(rows)
}

func (r *analysesRepo) list(ctx context.Context, where string, args ...any) ([]*analysis.AnalysisResult, error) {
	rows, err := r.q.Query(ctx, "SELECT "+analysisSelectCols+" FROM analysis "+where, args...)
	if err != nil {
		return nil, classify("list analyses", err)
	}
	defer rows.Close()
	var out []*analysis.AnalysisResult
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list analyses: rows", err)
	}
	return out, nil
}

func scanAnalysis(rs rowScanner) (*analysis.AnalysisResult, error) {
	var (
		rawID           uuid.UUID
		kindStr         string
		rawPR           *uuid.UUID
		rawSprint       *uuid.UUID
		rawRelease      *uuid.UUID
		analyzerName    string
		analyzerVersion string
		promptVersion   string
		payloadB        []byte
		model           string
		tokensUsed      int
		generatedAt     time.Time
	)
	if err := rs.Scan(
		&rawID, &kindStr,
		&rawPR, &rawSprint, &rawRelease,
		&analyzerName, &analyzerVersion, &promptVersion,
		&payloadB, &model, &tokensUsed, &generatedAt,
	); err != nil {
		return nil, classify("scan analysis", err)
	}

	id, err := shared.ParseAnalysisID(rawID.String())
	if err != nil {
		return nil, classify("scan analysis: corrupt id", err)
	}
	kind := analysis.Kind(kindStr)
	if !kind.Valid() {
		return nil, classify("scan analysis: invalid kind",
			errors.New("unknown kind "+kindStr))
	}

	subject, err := rehydrateSubject(rawPR, rawSprint, rawRelease)
	if err != nil {
		return nil, classify("scan analysis: subject", err)
	}

	payload, err := analysis.UnmarshalPayload(kind, payloadB)
	if err != nil {
		return nil, classify("scan analysis: payload", err)
	}

	return analysis.Rehydrate(
		id,
		kind,
		subject,
		analysis.AnalyzerRef{
			Name:          analyzerName,
			Version:       analyzerVersion,
			PromptVersion: promptVersion,
		},
		payload,
		model,
		tokensUsed,
		generatedAt.UTC(),
	), nil
}

func rehydrateSubject(rawPR, rawSprint, rawRelease *uuid.UUID) (analysis.Subject, error) {
	count := 0
	if rawPR != nil {
		count++
	}
	if rawSprint != nil {
		count++
	}
	if rawRelease != nil {
		count++
	}
	if count != 1 {
		return analysis.Subject{}, fmt.Errorf("subject must reference exactly one entity, got %d", count)
	}
	switch {
	case rawPR != nil:
		prID, err := shared.ParsePullRequestID(rawPR.String())
		if err != nil {
			return analysis.Subject{}, err
		}
		return analysis.SubjectForPR(prID), nil
	case rawSprint != nil:
		sprintID, err := shared.ParseSprintID(rawSprint.String())
		if err != nil {
			return analysis.Subject{}, err
		}
		return analysis.SubjectForSprint(sprintID), nil
	default:
		relID, err := shared.ParseReleaseID(rawRelease.String())
		if err != nil {
			return analysis.Subject{}, err
		}
		return analysis.SubjectForRelease(relID), nil
	}
}
