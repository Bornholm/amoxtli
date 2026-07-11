package gorm

import (
	"time"

	"github.com/bornholm/amoxtli/model"
)

type Collection struct {
	ID string `gorm:"primaryKey;autoIncrement:false"`

	CreatedAt time.Time
	UpdatedAt time.Time

	Label       string
	Description string

	Documents []*Document `gorm:"many2many:documents_collections;constraint:OnDelete:CASCADE"`
}

type wrappedCollection struct {
	c *Collection
}

// CreatedAt implements [model.PersistedCollection].
func (w *wrappedCollection) CreatedAt() time.Time {
	return w.c.CreatedAt
}

// UpdatedAt implements [model.PersistedCollection].
func (w *wrappedCollection) UpdatedAt() time.Time {
	return w.c.UpdatedAt
}

// Description implements model.Collection.
func (w *wrappedCollection) Description() string {
	return w.c.Description
}

// ID implements model.Collection.
func (w *wrappedCollection) ID() model.CollectionID {
	return model.CollectionID(w.c.ID)
}

// Label implements model.Collection.
func (w *wrappedCollection) Label() string {
	return w.c.Label
}

var _ model.PersistedCollection = &wrappedCollection{}

func fromCollection(c model.Collection) *Collection {
	collection := &Collection{
		ID:          string(c.ID()),
		Label:       c.Label(),
		Description: c.Description(),
	}

	return collection
}
