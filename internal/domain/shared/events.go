package shared

import "time"

// DomainEvent is the contract every domain event implements. Events are
// raised by aggregates on state transitions and consumed by the
// application layer (typically via an outbox/publisher).
type DomainEvent interface {
	EventName() string
	OccurredAt() time.Time
	AggregateID() string
}

// BaseEvent is an embeddable helper that satisfies the common fields of
// DomainEvent. Concrete events still must implement EventName().
type BaseEvent struct {
	occurredAt time.Time
	aggregate  string
}

// NewBaseEvent constructs a BaseEvent. Aggregates should call this from
// their state-mutating methods using the injected Clock.
func NewBaseEvent(aggregateID string, at time.Time) BaseEvent {
	return BaseEvent{occurredAt: at, aggregate: aggregateID}
}

// OccurredAt implements DomainEvent.
func (b BaseEvent) OccurredAt() time.Time { return b.occurredAt }

// AggregateID implements DomainEvent.
func (b BaseEvent) AggregateID() string { return b.aggregate }

// EventEmitter is a tiny in-aggregate buffer of events. Aggregates embed
// it (or hold one privately) and expose PullEvents() so the application
// layer can drain after a successful save.
type EventEmitter struct {
	events []DomainEvent
}

// Record appends an event to the emitter.
func (e *EventEmitter) Record(ev DomainEvent) {
	e.events = append(e.events, ev)
}

// PullEvents returns and clears the recorded events. Idempotent — a
// subsequent call returns nil.
func (e *EventEmitter) PullEvents() []DomainEvent {
	out := e.events
	e.events = nil
	return out
}
