package vision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/bornholm/amoxtli/telemetry"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// CachingDescriber decorates a Describer with a persistent on-disk cache keyed
// by the *content* of the image — sha256(promptVersion, namespace, bytes) —
// sharded on the first hex byte below dir/vision, the same layout as
// llmx.CachingClient.
//
// A dedicated cache is required: llmx.CachingClient deliberately refuses to
// cache any chat completion carrying an attachment (serializing the image into
// a JSON key would be wasteful), so vision calls would otherwise never be
// cached. Keying on the bytes rather than on the file path also means a
// renamed, moved or duplicated image is described only once, and a full
// re-index costs nothing.
//
// The namespace must identify the vision model *and* its prompt — build it
// with Namespace(model, prompt) — otherwise switching model would serve
// descriptions produced by the previous one.
type CachingDescriber struct {
	inner     Describer
	dir       string
	namespace string

	hits   atomic.Int64
	misses atomic.Int64
}

// NewCachingDescriber wraps describer with a cache rooted at dir (created if
// missing), keyed under namespace.
func NewCachingDescriber(describer Describer, dir, namespace string) (*CachingDescriber, error) {
	if dir == "" {
		return nil, errors.New("vision: cache directory must not be empty")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errors.WithStack(err)
	}

	return &CachingDescriber{
		inner:     describer,
		dir:       dir,
		namespace: namespace,
	}, nil
}

// Stats returns the number of cache hits and misses served so far.
func (c *CachingDescriber) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// MaxImageBytes reports the limit of the wrapped describer, when it exposes
// one, so the decorator stays transparent to size-aware callers.
func (c *CachingDescriber) MaxImageBytes() int64 {
	if inner, ok := c.inner.(interface{ MaxImageBytes() int64 }); ok {
		return inner.MaxImageBytes()
	}

	return DefaultMaxImageBytes
}

// Describe implements Describer.
func (c *CachingDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*Description, error) {
	path := c.path(data)

	if desc, ok := c.load(path); ok {
		c.hits.Add(1)
		recordCacheLookup(ctx, telemetry.VisionCacheHit)

		return desc, nil
	}

	c.misses.Add(1)
	recordCacheLookup(ctx, telemetry.VisionCacheMiss)

	desc, err := c.inner.Describe(ctx, mimeType, data)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Writing is best-effort: a cache we could not persist must never fail a
	// description we already paid for.
	c.store(path, desc)

	return desc, nil
}

// recordCacheLookup counts one description cache lookup.
func recordCacheLookup(ctx context.Context, result string) {
	if counter := telemetry.Metrics().VisionCacheLookups; counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.AttrVisionCacheResult, result)))
	}
}

// path derives the cache file path of an image: sha256 over the prompt
// version, the namespace and the image bytes — independent of the file name,
// stable across re-indexations.
func (c *CachingDescriber) path(data []byte) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00", PromptVersion, c.namespace)
	h.Write(data)
	key := hex.EncodeToString(h.Sum(nil))

	return filepath.Join(c.dir, "vision", key[:2], key+".json")
}

// load reads a cached description; any error (missing, corrupted) is a
// silent miss.
func (c *CachingDescriber) load(path string) (*Description, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var desc Description
	if err := json.Unmarshal(data, &desc); err != nil || desc.IsEmpty() {
		return nil, false
	}

	return &desc, true
}

// store writes a cache entry atomically (temp file + rename), so concurrent
// readers never observe a partial entry.
func (c *CachingDescriber) store(path string, desc *Description) {
	if desc.IsEmpty() {
		return
	}

	data, err := json.Marshal(desc)
	if err != nil {
		return
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

var _ Describer = &CachingDescriber{}
