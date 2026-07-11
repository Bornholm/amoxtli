package pipeline

import (
	"github.com/bornholm/amoxtli/index"
)

type IdentifiedIndex struct {
	id    string
	index index.Index
}

func (i *IdentifiedIndex) Index() index.Index {
	return i.index
}

func (i *IdentifiedIndex) ID() string {
	return i.id
}

func NewIdentifiedIndex(id string, index index.Index) *IdentifiedIndex {
	return &IdentifiedIndex{
		id:    id,
		index: index,
	}
}
