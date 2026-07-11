package workflow

import (
	"context"

	"github.com/pkg/errors"
)

type Step interface {
	Execute(ctx context.Context) error
	Compensate(ctx context.Context) error
}

type step struct {
	execute    func(ctx context.Context) error
	compensate func(ctx context.Context) error
}

// Compensate implements Step.
func (s *step) Compensate(ctx context.Context) error {
	if s.compensate == nil {
		return nil
	}

	if err := s.compensate(ctx); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Execute implements Step.
func (s *step) Execute(ctx context.Context) error {
	if s.execute == nil {
		return nil
	}

	if err := s.execute(ctx); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

var _ Step = &step{}

func StepFunc(execute func(ctx context.Context) error, compensate func(ctx context.Context) error) Step {
	return &step{
		execute:    execute,
		compensate: compensate,
	}
}
