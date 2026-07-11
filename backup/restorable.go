package backup

import (
	"context"

	"github.com/bornholm/amoxtli/model"
)

type Restorable interface {
	RestoreDocuments(ctx context.Context, documents []model.Document) error
}
