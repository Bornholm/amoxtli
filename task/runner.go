package task

import (
	"context"
	"time"
)

type Status string

const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

type StateHeader struct {
	ID          ID
	Type        Type
	ScheduledAt time.Time
	Status      Status
}

type State struct {
	StateHeader
	FinishedAt time.Time
	Progress   float32
	Error      error
	Message    string
}

type Event struct {
	Message  *string
	Progress *float32
}

type EventFunc func(p *Event)

func WithMessage(message string) EventFunc {
	return func(p *Event) {
		p.Message = &message
	}
}

func WithProgress(progress float32) EventFunc {
	return func(p *Event) {
		p.Progress = &progress
	}
}

func NewEvent(funcs ...EventFunc) Event {
	p := Event{}
	for _, fn := range funcs {
		fn(&p)
	}
	return p
}

type Handler interface {
	Handle(ctx context.Context, task Task, events chan Event) error
}

type HandlerFunc func(ctx context.Context, task Task, events chan Event) error

func (f HandlerFunc) Handle(ctx context.Context, task Task, events chan Event) error {
	return f(ctx, task, events)
}

type Runner interface {
	ScheduleTask(ctx context.Context, task Task) error
	GetTaskState(ctx context.Context, id ID) (*State, error)
	GetTask(ctx context.Context, id ID) (Task, error)
	ListTasks(ctx context.Context) ([]StateHeader, error)
	RegisterTask(taskType Type, handler Handler)
	// CancelTask cancels a scheduled or running task
	// A canceled task should return the error ErrCanceled
	CancelTask(ctx context.Context, id ID) error
	Run(ctx context.Context) error
}

// Factory rebuilds a concrete task from its persisted data.
type Factory func(id ID, payload []byte) (Task, error)

// PersistentRunner is an extension of Runner that supports
// registering deserialization factories for persistence.
type PersistentRunner interface {
	RegisterFactory(taskType Type, factory Factory)
}
