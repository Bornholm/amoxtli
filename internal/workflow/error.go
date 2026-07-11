package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

type CompensationError struct {
	executionErr     error
	compensationErrs []error
}

func (e *CompensationError) ExecutionError() error {
	return e.executionErr
}

func (e *CompensationError) CompensationErrors() []error {
	return e.compensationErrs
}

func (e *CompensationError) Error() string {
	var sb strings.Builder

	sb.WriteString("compensation error: ")
	sb.WriteString("execution error '")
	sb.WriteString(fmt.Sprintf("%+v", e.ExecutionError()))
	sb.WriteString("' resulted in following compensation errors: ")

	for idx, err := range e.CompensationErrors() {
		if idx > 0 {
			sb.WriteString(", ")
		}

		sb.WriteString("[")
		sb.WriteString(strconv.FormatInt(int64(idx), 10))
		sb.WriteString("] ")
		sb.WriteString(fmt.Sprintf("%+v", err))
	}

	return sb.String()
}

func NewCompensationError(executionErr error, compensationErrs ...error) *CompensationError {
	return &CompensationError{
		executionErr:     executionErr,
		compensationErrs: compensationErrs,
	}
}

var _ error = &CompensationError{}
