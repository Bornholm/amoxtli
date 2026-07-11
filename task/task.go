package task

import (
	"encoding/json"

	"github.com/rs/xid"
)

type ID string

func NewID() ID {
	return ID(xid.New().String())
}

type Type string

type Task interface {
	json.Marshaler
	json.Unmarshaler

	ID() ID
	Type() Type
}
