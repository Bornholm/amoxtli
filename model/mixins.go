package model

import (
	"time"
)

type WithID[T ~string] interface {
	ID() T
}

type WithLifecycle interface {
	CreatedAt() time.Time
	UpdatedAt() time.Time
}
