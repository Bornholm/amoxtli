package workflow

import (
	"context"

	"github.com/pkg/errors"
)

type Workflow struct {
	steps []Step
}

func (w *Workflow) Execute(ctx context.Context) error {
	for idx, step := range w.steps {
		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		default:
		}

		if executionErr := step.Execute(ctx); executionErr != nil {
			if compensationErrs := w.compensate(context.WithoutCancel(ctx), idx); compensationErrs != nil {
				return errors.WithStack(NewCompensationError(executionErr, compensationErrs...))
			}

			return errors.WithStack(executionErr)
		}
	}

	return nil
}

func (w *Workflow) compensate(ctx context.Context, fromIndex int) []error {
	errs := make([]error, 0)
	for idx := fromIndex; idx >= 0; idx -= 1 {
		act := w.steps[idx]

		if err := act.Compensate(ctx); err != nil {
			errs = append(errs, errors.WithStack(err))
		}
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

func New(steps ...Step) *Workflow {
	return &Workflow{steps: steps}
}
