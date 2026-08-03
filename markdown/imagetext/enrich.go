// Package imagetext makes the images embedded in a markdown document
// searchable: each resolvable image is described by a vision LLM and the
// description is inserted, as plain text, right after the block carrying the
// image.
//
// The insertion is a rewrite of the *source bytes*, not an AST mutation:
// markdown.Document keeps the raw source and every section references a byte
// range in it (see markdown/parser.go), so only a splice of the source ends up
// indexed. Because the description lands in the same block as the image, it
// falls in the same section — contextualized by the surrounding headings,
// exactly what full-text search and grounding need.
//
// Enrichment is best-effort by design: an image that cannot be resolved or
// described is left with its alt-text alone and indexing continues. Only a
// context error (cancellation, timeout) fails the document.
package imagetext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"golang.org/x/sync/errgroup"
)

// Enrich returns data with the description of each resolvable image inserted
// after the block carrying it. The returned slice is data itself when nothing
// could be described.
func Enrich(ctx context.Context, data []byte, funcs ...OptionFunc) ([]byte, error) {
	opts := NewOptions(funcs...)

	if (opts.Describer == nil && opts.Blobs == nil) || len(data) == 0 {
		return data, nil
	}

	occurrences := collectImages(data)
	if len(occurrences) == 0 {
		return data, nil
	}

	images, err := resolveImages(ctx, occurrences, opts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if len(images) == 0 {
		return data, nil
	}

	descriptions, err := describeImages(ctx, images, opts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	uris := storeImages(ctx, images, opts)

	if len(descriptions) == 0 && len(uris) == 0 {
		return data, nil
	}

	return splice(data, occurrences, descriptions, uris), nil
}

// storeImages persists the resolved images and returns the internal URI of
// each, so the splice can point the document at the store instead of at a
// local path or a base64 payload. Storing is best-effort: a failure costs the
// reference, never the description.
func storeImages(ctx context.Context, images map[string]*resolvedImage, opts *Options) map[string]string {
	if opts.Blobs == nil {
		return nil
	}

	uris := make(map[string]string, len(images))

	for hash, image := range images {
		stored, err := opts.Blobs.Put(ctx, image.mimeType, image.data)
		if err != nil {
			slog.WarnContext(ctx, "could not store image, keeping its original reference",
				slog.Any("error", errors.WithStack(err)),
			)

			continue
		}

		uris[hash] = blob.URI(stored)
	}

	return uris
}

// Enricher applies a fixed set of options to every document. It is the form
// the ingestion pipeline holds on to.
type Enricher struct {
	funcs []OptionFunc
}

// New builds an Enricher applying funcs to every document it enriches.
func New(funcs ...OptionFunc) *Enricher {
	return &Enricher{funcs: funcs}
}

// Enrich describes the images of a markdown document. baseDir is the directory
// relative image paths resolve against (empty disables their resolution) and
// progress, when non-nil, is called as descriptions complete. Both override the
// options the Enricher was built with — they are per-document values.
func (e *Enricher) Enrich(ctx context.Context, data []byte, baseDir string, progress func(done, total int)) ([]byte, error) {
	funcs := make([]OptionFunc, 0, len(e.funcs)+2)
	funcs = append(funcs, e.funcs...)
	funcs = append(funcs, WithBaseDir(baseDir))

	if progress != nil {
		funcs = append(funcs, WithProgress(progress))
	}

	return Enrich(ctx, data, funcs...)
}

// imageOccurrence is one `![alt](destination)` in the document, with the byte
// offset at which its description must be inserted.
type imageOccurrence struct {
	destination string
	// blockStart is the start offset of the block carrying the image; it bounds
	// the search for the destination in the source (see destinationSpan).
	blockStart int
	// insertAt is the end offset of the block carrying the image: inserting
	// there keeps the description inside the same section.
	insertAt int
	// hash identifies the resolved content; empty until resolution succeeds.
	hash string
}

// collectImages walks the document and returns its images in document order.
func collectImages(data []byte) []*imageOccurrence {
	root := markdown.New().Parser().Parse(text.NewReader(data))

	var occurrences []*imageOccurrence

	// The walk cannot fail: the callback never returns an error.
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		image, ok := n.(*ast.Image)
		if !ok {
			return ast.WalkContinue, nil
		}

		start, end, ok := blockRange(image)
		if !ok {
			return ast.WalkContinue, nil
		}

		occurrences = append(occurrences, &imageOccurrence{
			destination: string(image.Destination),
			blockStart:  start,
			insertAt:    end,
		})

		return ast.WalkContinue, nil
	})

	return occurrences
}

// blockRange returns the source range of the closest enclosing block owning
// source lines (the paragraph or heading carrying the image).
func blockRange(n ast.Node) (start, end int, ok bool) {
	for current := n; current != nil; current = current.Parent() {
		if current.Type() != ast.TypeBlock {
			continue
		}

		lines := current.Lines()
		if lines.Len() == 0 {
			// A container block (blockquote, list item) delegates its lines to
			// its children: keep climbing.
			continue
		}

		return lines.At(0).Start, lines.At(lines.Len() - 1).Stop, true
	}

	return 0, 0, false
}

// resolveImages resolves the occurrences to their bytes, deduplicating by
// content hash and honouring the per-document cap. Occurrences that resolve
// are tagged with their hash in place.
func resolveImages(ctx context.Context, occurrences []*imageOccurrence, opts *Options) (map[string]*resolvedImage, error) {
	images := make(map[string]*resolvedImage)
	byDestination := make(map[string]string, len(occurrences))

	for _, occurrence := range occurrences {
		// The very same destination twice: no need to resolve it again.
		if hash, exists := byDestination[occurrence.destination]; exists {
			occurrence.hash = hash
			continue
		}

		resolved, err := resolve(ctx, occurrence.destination, opts)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errors.WithStack(ctx.Err())
			}

			slog.DebugContext(ctx, "skipping markdown image",
				slog.String("destination", truncateDestination(occurrence.destination)),
				slog.Any("reason", err),
			)

			continue
		}

		sum := sha256.Sum256(resolved.data)
		hash := hex.EncodeToString(sum[:])

		// Distinct occurrences of the same image share one description.
		if _, exists := images[hash]; !exists {
			if len(images) >= opts.MaxImages {
				slog.WarnContext(ctx, "too many images in document, keeping alt-text only for the remaining ones",
					slog.Int("maxImages", opts.MaxImages),
				)

				break
			}

			images[hash] = resolved
		}

		byDestination[occurrence.destination] = hash
		occurrence.hash = hash
	}

	return images, nil
}

// describeImages describes each distinct image, with a bounded concurrency. A
// failing description drops the image, not the document.
func describeImages(ctx context.Context, images map[string]*resolvedImage, opts *Options) (map[string]*vision.Description, error) {
	// A blob store alone (no describer) is a legitimate configuration: store
	// and reference the images without paying for a description.
	if opts.Describer == nil {
		return nil, nil
	}

	// Describe in a stable order so a cancelled or partial run is reproducible.
	hashes := make([]string, 0, len(images))
	for hash := range images {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)

	var (
		mu           sync.Mutex
		done         int
		descriptions = make(map[string]*vision.Description, len(hashes))
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(opts.Concurrency)

	for _, hash := range hashes {
		group.Go(func() error {
			image := images[hash]

			description, err := opts.Describer.Describe(groupCtx, image.mimeType, image.data)

			mu.Lock()
			defer mu.Unlock()

			done++

			if opts.Progress != nil {
				opts.Progress(done, len(hashes))
			}

			if err != nil {
				// Only a cancellation must stop the document; a provider error
				// on one image leaves that image with its alt-text.
				if groupCtx.Err() != nil {
					return errors.WithStack(err)
				}

				// A format no vision provider accepts (SVG, BMP, ICO...) is an
				// expected outcome of a real corpus, not an incident: the image
				// keeps its alt-text without polluting the logs.
				if errors.Is(err, vision.ErrUnsupportedImageFormat) {
					slog.DebugContext(groupCtx, "image format not supported by the vision model, keeping alt-text only",
						slog.String("mimeType", image.mimeType),
					)

					return nil
				}

				slog.WarnContext(groupCtx, "could not describe image, keeping alt-text only",
					slog.Any("error", errors.WithStack(err)),
				)

				return nil
			}

			if !description.IsEmpty() {
				descriptions[hash] = description
			}

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	return descriptions, nil
}

// splice rebuilds the source with, for each image, its description inserted
// after the block carrying it and — when the image was stored — its
// destination rewritten to the internal blob URI. Both rewrites happen in the
// same pass: doing them separately would invalidate the offsets of the other.
func splice(data []byte, occurrences []*imageOccurrence, descriptions map[string]*vision.Description, uris map[string]string) []byte {
	edits := make([]edit, 0, 2*len(occurrences))

	// cursors advances the destination search per block so that repeated
	// destinations inside one block map to distinct spans.
	cursors := make(map[int]int, len(occurrences))

	for _, occurrence := range occurrences {
		if occurrence.hash == "" {
			continue
		}

		if description, exists := descriptions[occurrence.hash]; exists {
			if occurrence.insertAt >= 0 && occurrence.insertAt <= len(data) {
				edits = append(edits, edit{
					start: occurrence.insertAt,
					end:   occurrence.insertAt,
					text:  format(description),
				})
			}
		}

		uri, stored := uris[occurrence.hash]
		if !stored {
			continue
		}

		cursor := max(cursors[occurrence.blockStart], occurrence.blockStart)

		start, end, ok := destinationSpan(data, cursor, occurrence.insertAt, occurrence.destination)
		if !ok {
			// The destination could not be located in the source (an escaped or
			// angle-bracketed form): the description still lands, the reference
			// simply stays as it was.
			continue
		}

		cursors[occurrence.blockStart] = end

		edits = append(edits, edit{start: start, end: end, text: uri})
	}

	return applyEdits(data, edits)
}

// format renders a description as a single-line block quote. It must stay on
// one line: a raw newline would break out of the quote and, worse, shift the
// block structure of the document around it.
func format(description *vision.Description) string {
	var buf strings.Builder

	buf.WriteString("\n\n> **Image")

	if title := singleLine(description.Title); title != "" {
		buf.WriteString(" — ")
		buf.WriteString(title)
	}

	buf.WriteString("**")

	if body := singleLine(description.Description); body != "" {
		buf.WriteString(" : ")
		buf.WriteString(body)
	}

	if visible := singleLine(description.Text); visible != "" {
		buf.WriteString(" ")
		buf.WriteString(visible)
	}

	// No trailing newline: the insertion point is the end of the last line of
	// the block, *before* its own line break, which terminates the quote.
	return buf.String()
}

// singleLine collapses every run of whitespace so the value fits on one
// markdown line.
func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// truncateDestination keeps a data URI from flooding the logs.
func truncateDestination(destination string) string {
	const max = 96

	if len(destination) <= max {
		return destination
	}

	return destination[:max] + "…"
}
