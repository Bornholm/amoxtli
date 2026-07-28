package imagetext

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/bornholm/amoxtli/blob"
	"github.com/pkg/errors"
)

// InlineLocalImages rewrites, in a markdown source, every image destination
// pointing to a readable file under dir so that it no longer depends on that
// directory. It is what makes the output of a document converter autonomous:
// pandoc extracts the media of a .docx to a temporary directory that is
// deleted as soon as the conversion returns, so the bytes must leave with the
// markdown.
//
// With a blob store (InlineLocalImagesWithStore) the media are stored and
// referenced by their internal URI — a much lighter markdown, and no double
// storage downstream. Without one, they are inlined as `data:` URIs.
//
// Destinations that cannot be handled — outside dir, absolute, missing, not an
// image, or larger than maxBytes — are left untouched: they are links to files
// that will simply be ignored (and stripped from the rendered chunks)
// downstream. maxBytes <= 0 defers to DefaultMaxImageBytes.
//
// The rewrite is a splice of the source bytes, like Enrich: the markdown
// structure and every other byte of the document are preserved.
func InlineLocalImages(data []byte, dir string, maxBytes int64) ([]byte, error) {
	return InlineLocalImagesWithStore(context.Background(), data, dir, maxBytes, nil)
}

// InlineLocalImagesWithStore is InlineLocalImages storing the media in a blob
// store, when one is given, instead of inlining them as data URIs.
func InlineLocalImagesWithStore(ctx context.Context, data []byte, dir string, maxBytes int64, blobs blob.Store) ([]byte, error) {
	if len(data) == 0 || dir == "" {
		return data, nil
	}

	opts := NewOptions(
		WithBaseDir(dir),
		WithMaxImageBytes(maxBytes),
		// Inlining is not describing: a small image is still content the
		// enrichment may later decide to skip on its own.
		WithMinDimension(-1),
	)

	occurrences := collectImages(data)
	if len(occurrences) == 0 {
		return data, nil
	}

	var (
		edits []edit
		// cursors advances the search per block so that repeated destinations
		// inside one block map to distinct spans.
		cursors  = make(map[int]int, len(occurrences))
		rewrites = make(map[string]string, len(occurrences))
		refused  = make(map[string]bool, len(occurrences))
		unparsed int
	)

	for _, occurrence := range occurrences {
		if refused[occurrence.destination] {
			continue
		}

		// Only local, relative destinations are candidates; a data URI is
		// already inline and a remote one is left to the reader.
		if strings.HasPrefix(occurrence.destination, "data:") {
			continue
		}

		value, exists := rewrites[occurrence.destination]
		if !exists {
			resolved, err := resolveLocal(occurrence.destination, opts)
			if err != nil {
				refused[occurrence.destination] = true

				slog.Debug("keeping markdown image as a link",
					slog.String("destination", truncateDestination(occurrence.destination)),
					slog.Any("reason", err),
				)

				continue
			}

			if blobs != nil {
				hash, err := blobs.Put(ctx, resolved.mimeType, resolved.data)
				if err != nil {
					refused[occurrence.destination] = true

					slog.WarnContext(ctx, "could not store extracted media, keeping it as a link",
						slog.String("destination", truncateDestination(occurrence.destination)),
						slog.Any("error", errors.WithStack(err)),
					)

					continue
				}

				value = blob.URI(hash)
			} else {
				value = "data:" + resolved.mimeType + ";base64," +
					base64.StdEncoding.EncodeToString(resolved.data)
			}

			rewrites[occurrence.destination] = value
		}

		cursor := max(cursors[occurrence.blockStart], occurrence.blockStart)

		start, end, ok := destinationSpan(data, cursor, occurrence.insertAt, occurrence.destination)
		if !ok {
			// The destination could not be located in the source (an escaped or
			// angle-bracketed form): leaving the link alone is the safe outcome.
			unparsed++
			continue
		}

		cursors[occurrence.blockStart] = end

		edits = append(edits, edit{start: start, end: end, text: value})
	}

	if unparsed > 0 {
		slog.Debug("some image destinations could not be located in the source",
			slog.Int("count", unparsed),
		)
	}

	return applyEdits(data, edits), nil
}

// destinationSpan locates the byte range of destination inside the inline link
// syntax `](destination)` — the only place a rewrite is safe — searching
// between from and to. goldmark keeps no source segment for a link
// destination, so it has to be found back in the source.
func destinationSpan(data []byte, from, to int, destination string) (start, end int, ok bool) {
	if from < 0 || to > len(data) || from >= to || destination == "" {
		return 0, 0, false
	}

	needle := []byte("](" + destination)
	window := data[from:to]

	for offset := 0; ; {
		index := bytes.Index(window[offset:], needle)
		if index < 0 {
			return 0, 0, false
		}

		index += offset

		// What follows the destination must close the link, or open its
		// optional title — otherwise the match is a longer destination sharing
		// this prefix.
		after := index + len(needle)
		if after < len(window) {
			switch window[after] {
			case ')', ' ', '\t':
				start = from + index + len("](")
				return start, start + len(destination), true
			}
		}

		offset = index + len(needle)
		if offset >= len(window) {
			return 0, 0, false
		}
	}
}
