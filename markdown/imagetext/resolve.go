package imagetext

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	// Header decoders for the dimension filter. WebP is not in the standard
	// library but is accepted by the vision providers, so it is pulled from
	// golang.org/x/image.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// errSkip marks an image the enrichment deliberately leaves alone (too small,
// too large, unresolvable, unsupported scheme). It never fails a document: the
// image keeps its alt-text and indexing continues.
var errSkip = errors.New("image skipped")

// resolvedImage is an image whose bytes were obtained and validated.
type resolvedImage struct {
	mimeType string
	data     []byte
}

// resolve turns a markdown image destination into its bytes. Every failure is
// wrapped in errSkip except context errors, which must abort the document.
func resolve(ctx context.Context, destination string, opts *Options) (*resolvedImage, error) {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return nil, errors.Wrap(errSkip, "empty destination")
	}

	switch {
	case strings.HasPrefix(destination, "data:"):
		return resolveDataURL(destination, opts)
	case strings.HasPrefix(destination, "http://"), strings.HasPrefix(destination, "https://"):
		return resolveRemote(ctx, destination, opts)
	default:
		return resolveLocal(destination, opts)
	}
}

// resolveDataURL decodes an inline data: image — the form produced by the
// document converters that inline their media.
func resolveDataURL(destination string, opts *Options) (*resolvedImage, error) {
	rest := strings.TrimPrefix(destination, "data:")

	header, payload, found := strings.Cut(rest, ",")
	if !found {
		return nil, errors.Wrap(errSkip, "malformed data url")
	}

	mimeType, isBase64 := "", false
	for i, part := range strings.Split(header, ";") {
		switch {
		case part == "base64":
			isBase64 = true
		case i == 0 && part != "":
			mimeType = part
		}
	}

	if !isBase64 {
		return nil, errors.Wrap(errSkip, "unsupported data url encoding")
	}

	if !isImageMimeType(mimeType) {
		return nil, errors.Wrapf(errSkip, "unsupported data url media type '%s'", mimeType)
	}

	// A base64 payload is 4/3 of the decoded size: bound it before decoding
	// rather than after, so an oversized inline image never lands in memory
	// twice.
	if int64(len(payload)) > (opts.MaxImageBytes/3+1)*4 {
		return nil, errors.Wrap(errSkip, "image exceeds the size limit")
	}

	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, errors.Wrap(errSkip, "invalid base64 payload")
	}

	return validate(mimeType, data, opts)
}

// resolveRemote fetches an http(s) destination, which requires an explicitly
// configured fetcher: ingestion performs no network call by default.
func resolveRemote(ctx context.Context, destination string, opts *Options) (*resolvedImage, error) {
	if opts.HTTPFetcher == nil {
		return nil, errors.Wrap(errSkip, "remote images are not fetched")
	}

	mimeType, data, err := opts.HTTPFetcher.Fetch(ctx, destination)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.WithStack(err)
		}
		return nil, errors.Wrap(errSkip, err.Error())
	}

	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	if !isImageMimeType(mimeType) {
		return nil, errors.Wrapf(errSkip, "unsupported media type '%s'", mimeType)
	}

	return validate(mimeType, data, opts)
}

// resolveLocal reads an image from the filesystem. The resolved path is
// strictly confined to the base directory: absolute paths and any traversal
// out of it are refused, whatever the document claims.
func resolveLocal(destination string, opts *Options) (*resolvedImage, error) {
	if opts.BaseDir == "" {
		return nil, errors.Wrap(errSkip, "no base directory configured")
	}

	// A markdown destination is a URL: drop the fragment/query and undo the
	// percent-encoding before touching the filesystem.
	parsed, err := url.Parse(destination)
	if err != nil {
		return nil, errors.Wrap(errSkip, "malformed destination")
	}

	if parsed.Scheme != "" || parsed.Host != "" {
		return nil, errors.Wrapf(errSkip, "unsupported destination scheme '%s'", parsed.Scheme)
	}

	path := parsed.Path
	if path == "" {
		return nil, errors.Wrap(errSkip, "empty destination path")
	}

	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return nil, errors.Wrap(errSkip, "absolute paths are refused")
	}

	baseDir, err := filepath.Abs(opts.BaseDir)
	if err != nil {
		return nil, errors.Wrap(errSkip, "unusable base directory")
	}

	resolved := filepath.Join(baseDir, filepath.FromSlash(path))

	relative, err := filepath.Rel(baseDir, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.Wrapf(errSkip, "path '%s' escapes the base directory", path)
	}

	mimeType := mimeTypeByExtension(filepath.Ext(resolved))
	if !isImageMimeType(mimeType) {
		return nil, errors.Wrapf(errSkip, "unsupported media type '%s'", mimeType)
	}

	file, err := os.Open(resolved)
	if err != nil {
		return nil, errors.Wrapf(errSkip, "could not open '%s'", path)
	}
	defer file.Close()

	// One byte past the limit: an oversized file is refused without being fully
	// read into memory.
	data, err := io.ReadAll(io.LimitReader(file, opts.MaxImageBytes+1))
	if err != nil {
		return nil, errors.Wrapf(errSkip, "could not read '%s'", path)
	}

	return validate(mimeType, data, opts)
}

// validate applies the filters that must run before any (billable) call to the
// model: size, then dimensions read from the image header alone.
func validate(mimeType string, data []byte, opts *Options) (*resolvedImage, error) {
	if len(data) == 0 {
		return nil, errors.Wrap(errSkip, "empty image")
	}

	if int64(len(data)) > opts.MaxImageBytes {
		return nil, errors.Wrap(errSkip, "image exceeds the size limit")
	}

	if opts.MinDimension > 0 {
		// Only the header is decoded, never the pixels. An undecodable header
		// is not a rejection: the format may simply have no Go decoder (SVG,
		// AVIF...) while the vision provider accepts it.
		if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			if config.Width < opts.MinDimension || config.Height < opts.MinDimension {
				return nil, errors.Wrapf(errSkip, "image is %dx%d, below the %dpx minimum", config.Width, config.Height, opts.MinDimension)
			}
		}
	}

	return &resolvedImage{mimeType: mimeType, data: data}, nil
}

// mimeTypeByExtension resolves a media type from a file extension.
func mimeTypeByExtension(ext string) string {
	raw := mime.TypeByExtension(strings.ToLower(ext))
	if raw == "" {
		return ""
	}

	media, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return raw
	}

	return media
}

func isImageMimeType(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/")
}
