package imagetext

import (
	"context"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/vision"
)

// DefaultMinDimension is the smallest side, in pixels, an image must have to
// be worth a description: below it an image is an icon, a bullet or a spacer,
// never content.
const DefaultMinDimension = 64

// DefaultMaxImagesPerDocument bounds how many images of a single document are
// described. It is the main cost lever of the enrichment: a slide deck or a
// scanned manual can carry hundreds of images.
const DefaultMaxImagesPerDocument = 32

// DefaultConcurrency bounds how many descriptions of a single document are
// computed in parallel. The global rate limit of the LLM client protects the
// provider; this bound protects local memory (decoded images) and keeps a
// single document from starving the other ingestion tasks.
const DefaultConcurrency = 2

// DefaultMaxImageBytes is the largest image handled, both when describing and
// when inlining. It mirrors vision.DefaultMaxImageBytes: an image the model
// would refuse is not worth carrying around either.
const DefaultMaxImageBytes = vision.DefaultMaxImageBytes

// Fetcher resolves remote (http/https) image destinations. None is wired by
// default: ingestion performs no network call of its own.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (mimeType string, data []byte, err error)
}

// Options configures the enrichment of a markdown document.
type Options struct {
	// Describer produces the descriptions. Without one — and without a blob
	// store — Enrich is a no-op.
	Describer vision.Describer
	// Blobs, when set, stores every resolved image and makes Enrich rewrite
	// its destination to the internal blob URI.
	Blobs blob.Store
	// BaseDir is the directory relative image paths resolve against. Empty
	// disables the resolution of relative paths entirely.
	BaseDir string
	// MinDimension is the smallest accepted side, in pixels; <= 0 disables the
	// filter.
	MinDimension int
	// MaxImages bounds the number of distinct images described per document.
	MaxImages int
	// MaxImageBytes bounds the size of a single image.
	MaxImageBytes int64
	// Concurrency bounds the parallel descriptions of a single document.
	Concurrency int
	// HTTPFetcher resolves http(s) destinations; nil skips them.
	HTTPFetcher Fetcher
	// Progress, when set, is called as descriptions complete. It may be called
	// from several goroutines, but never concurrently with itself.
	Progress func(done, total int)
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		MinDimension:  DefaultMinDimension,
		MaxImages:     DefaultMaxImagesPerDocument,
		MaxImageBytes: DefaultMaxImageBytes,
		Concurrency:   DefaultConcurrency,
	}

	for _, fn := range funcs {
		fn(opts)
	}

	return opts
}

// WithDescriber sets the describer producing the image descriptions.
func WithDescriber(describer vision.Describer) OptionFunc {
	return func(opts *Options) {
		opts.Describer = describer
	}
}

// WithBlobStore stores every resolved image and rewrites its destination to
// the internal URI `amoxtli://images/<hash>`. Unlike a data URI, that
// destination survives the rendering of a chunk (StripDataURL only strips
// `data:`), so an agent reading a section sees the reference next to the
// description and can fetch the image back.
func WithBlobStore(store blob.Store) OptionFunc {
	return func(opts *Options) {
		opts.Blobs = store
	}
}

// WithBaseDir sets the directory relative image paths resolve against. Paths
// escaping it — and absolute paths — are always refused.
func WithBaseDir(dir string) OptionFunc {
	return func(opts *Options) {
		opts.BaseDir = dir
	}
}

// WithMinDimension ignores images whose smallest side is below n pixels
// (icons, bullets). 0 keeps the default (DefaultMinDimension); a negative
// value disables the filter.
func WithMinDimension(n int) OptionFunc {
	return func(opts *Options) {
		if n < 0 {
			opts.MinDimension = 0
			return
		}

		if n > 0 {
			opts.MinDimension = n
		}
	}
}

// WithMaxImagesPerDocument bounds the number of distinct images described in a
// single document; beyond it the remaining images keep their alt-text alone.
// <= 0 keeps the default.
func WithMaxImagesPerDocument(n int) OptionFunc {
	return func(opts *Options) {
		if n > 0 {
			opts.MaxImages = n
		}
	}
}

// WithMaxImageBytes bounds the size of a single image; <= 0 keeps the default.
func WithMaxImageBytes(maxBytes int64) OptionFunc {
	return func(opts *Options) {
		if maxBytes > 0 {
			opts.MaxImageBytes = maxBytes
		}
	}
}

// WithConcurrency bounds the parallel descriptions of a single document;
// <= 0 keeps the default.
func WithConcurrency(n int) OptionFunc {
	return func(opts *Options) {
		if n > 0 {
			opts.Concurrency = n
		}
	}
}

// WithHTTPFetcher enables the resolution of http(s) image destinations. It is
// deliberately not wired by the CLI: indexing a document must not trigger
// network calls to arbitrary hosts.
func WithHTTPFetcher(fetcher Fetcher) OptionFunc {
	return func(opts *Options) {
		opts.HTTPFetcher = fetcher
	}
}

// WithProgress reports the description progress of a document.
func WithProgress(fn func(done, total int)) OptionFunc {
	return func(opts *Options) {
		opts.Progress = fn
	}
}
