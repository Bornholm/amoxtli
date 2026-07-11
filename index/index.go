package index

import (
	"context"
	"net/url"

	"github.com/bornholm/amoxtli/model"
)

type Index interface {
	Index(ctx context.Context, document model.Document, funcs ...OptionFunc) error
	DeleteBySource(ctx context.Context, source *url.URL) error
	DeleteByID(ctx context.Context, ids ...model.SectionID) error
	All(ctx context.Context, yield func(model.SectionID) bool) error
	Search(ctx context.Context, query string, opts SearchOptions) ([]*SearchResult, error)
}

type Options struct {
	OnProgress func(progress float32)
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithOnProgress(onProgress func(progress float32)) OptionFunc {
	return func(opts *Options) {
		opts.OnProgress = onProgress
	}
}

type SearchOptions struct {
	MaxResults  int
	Collections []model.CollectionID
}

type SearchResult struct {
	Source   *url.URL
	Sections []model.SectionID
}
