package task

import "errors"

var (
	ErrNotFound  = errors.New("not found")
	ErrCanceled  = errors.New("canceled")
	ErrQueueFull = errors.New("queue full")
)
