package index

import (
	"context"
	"net/url"

	"github.com/pkg/errors"
)

// MetadataUpdater is an optional capability of an index keeping its own copy
// of document metadata (to evaluate filters natively). It replaces that copy
// without reindexing the document's content.
type MetadataUpdater interface {
	// UpdateMetadata replaces the metadata of the document with the given
	// source. An empty map removes it.
	UpdateMetadata(ctx context.Context, source *url.URL, metadata map[string]any) error
}

// UpdateMetadata replaces the metadata copy idx keeps for the document with
// the given source, unwrapping decorators until it finds a MetadataUpdater.
// An index without that capability keeps no metadata of its own, and there is
// nothing to update.
func UpdateMetadata(ctx context.Context, idx Index, source *url.URL, metadata map[string]any) error {
	current := idx
	for range maxUnwrapDepth {
		if updater, ok := current.(MetadataUpdater); ok {
			return errors.WithStack(updater.UpdateMetadata(ctx, source, metadata))
		}

		unwrapper, ok := current.(Unwrapper)
		if !ok {
			return nil
		}

		current = unwrapper.Unwrap()
		if current == nil {
			return nil
		}
	}

	return nil
}
