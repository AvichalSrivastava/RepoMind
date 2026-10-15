// Package repository contains the Repository aggregate — a tracked
// source-of-truth git repository whose pull requests, sprints, releases,
// and deployments RepoMind observes.
package repository

import (
	"fmt"
	"strings"
	"time"

	"github.com/repomind/repomind/internal/domain/shared"
)

// Repository is the aggregate root representing a tracked git repository.
//
// Invariants:
//   - Owner and Name are non-empty and form a unique full name.
//   - DefaultBranch, QABranch, UATBranch, and ProductionBranch are set
//     (defaults applied at registration).
//   - LastObservedAt is monotonically non-decreasing once set.
type Repository struct {
	id               shared.RepositoryID
	owner            string
	name             string
	defaultBranch    shared.Branch
	qaBranch         shared.Branch
	uatBranch        shared.Branch
	productionBranch shared.Branch
	createdAt        time.Time
	lastObservedAt   time.Time

	emitter shared.EventEmitter
}

// RegistrationInput holds the parameters required to register a new
// Repository. Branch fields are optional; sensible defaults are applied.
type RegistrationInput struct {
	Owner            string
	Name             string
	DefaultBranch    string // default: "main"
	QABranch         string // default: "qa"
	UATBranch        string // default: "uat"
	ProductionBranch string // default: "production"
}

// Register creates a brand-new Repository aggregate, validating inputs
// and emitting a Registered event.
func Register(in RegistrationInput, clock shared.Clock) (*Repository, error) {
	owner := strings.TrimSpace(in.Owner)
	name := strings.TrimSpace(in.Name)
	if owner == "" {
		return nil, shared.NewValidationError("owner", "must not be empty")
	}
	if name == "" {
		return nil, shared.NewValidationError("name", "must not be empty")
	}

	def, err := branchOrDefault(in.DefaultBranch, "main")
	if err != nil {
		return nil, err
	}
	qa, err := branchOrDefault(in.QABranch, "qa")
	if err != nil {
		return nil, err
	}
	uat, err := branchOrDefault(in.UATBranch, "uat")
	if err != nil {
		return nil, err
	}
	prod, err := branchOrDefault(in.ProductionBranch, "production")
	if err != nil {
		return nil, err
	}

	now := clock.Now()
	r := &Repository{
		id:               shared.NewRepositoryID(),
		owner:            owner,
		name:             name,
		defaultBranch:    def,
		qaBranch:         qa,
		uatBranch:        uat,
		productionBranch: prod,
		createdAt:        now,
	}
	r.emitter.Record(RegisteredEvent{
		BaseEvent: shared.NewBaseEvent(r.id.String(), now),
		FullName:  r.FullName(),
	})
	return r, nil
}

// Rehydrate reconstructs a Repository from persisted state. The
// repository implementation (Module 5) is the only caller.
func Rehydrate(
	id shared.RepositoryID,
	owner, name string,
	defaultBranch, qaBranch, uatBranch, productionBranch shared.Branch,
	createdAt, lastObservedAt time.Time,
) *Repository {
	return &Repository{
		id:               id,
		owner:            owner,
		name:             name,
		defaultBranch:    defaultBranch,
		qaBranch:         qaBranch,
		uatBranch:        uatBranch,
		productionBranch: productionBranch,
		createdAt:        createdAt,
		lastObservedAt:   lastObservedAt,
	}
}

// ---------- Accessors ----------

func (r *Repository) ID() shared.RepositoryID          { return r.id }
func (r *Repository) Owner() string                    { return r.owner }
func (r *Repository) Name() string                     { return r.name }
func (r *Repository) DefaultBranch() shared.Branch     { return r.defaultBranch }
func (r *Repository) QABranch() shared.Branch          { return r.qaBranch }
func (r *Repository) UATBranch() shared.Branch         { return r.uatBranch }
func (r *Repository) ProductionBranch() shared.Branch  { return r.productionBranch }
func (r *Repository) CreatedAt() time.Time             { return r.createdAt }
func (r *Repository) LastObservedAt() time.Time        { return r.lastObservedAt }
func (r *Repository) FullName() string                 { return r.owner + "/" + r.name }
func (r *Repository) PullEvents() []shared.DomainEvent { return r.emitter.PullEvents() }

// ---------- Behavior ----------

// MarkObserved records that the repository was successfully polled at
// the given time. Time cannot move backward.
func (r *Repository) MarkObserved(at time.Time) error {
	if !r.lastObservedAt.IsZero() && at.Before(r.lastObservedAt) {
		return fmt.Errorf("%w: cannot mark observed before last observation", shared.ErrInvariant)
	}
	r.lastObservedAt = at
	return nil
}

// RetargetBranches updates the tracked QA/UAT/Production branches —
// e.g. when an org renames `master` to `main`. Emits a Retargeted event.
func (r *Repository) RetargetBranches(def, qa, uat, prod shared.Branch, at time.Time) error {
	if def.IsZero() || qa.IsZero() || uat.IsZero() || prod.IsZero() {
		return fmt.Errorf("%w: all four branch refs must be supplied", shared.ErrInvalidArgument)
	}
	r.defaultBranch, r.qaBranch, r.uatBranch, r.productionBranch = def, qa, uat, prod
	r.emitter.Record(BranchesRetargetedEvent{
		BaseEvent:  shared.NewBaseEvent(r.id.String(), at),
		Default:    def.String(),
		QA:         qa.String(),
		UAT:        uat.String(),
		Production: prod.String(),
	})
	return nil
}

// ---------- helpers ----------

func branchOrDefault(input, fallback string) (shared.Branch, error) {
	if strings.TrimSpace(input) == "" {
		input = fallback
	}
	return shared.NewBranch(input)
}

// ---------- Events ----------

// RegisteredEvent fires when a new Repository is registered.
type RegisteredEvent struct {
	shared.BaseEvent
	FullName string
}

// EventName implements shared.DomainEvent.
func (RegisteredEvent) EventName() string { return "repository.registered" }

// BranchesRetargetedEvent fires when tracked branches change.
type BranchesRetargetedEvent struct {
	shared.BaseEvent
	Default    string
	QA         string
	UAT        string
	Production string
}

// EventName implements shared.DomainEvent.
func (BranchesRetargetedEvent) EventName() string { return "repository.branches_retargeted" }
