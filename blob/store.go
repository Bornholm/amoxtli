// Package blob stores the binary content amoxtli needs to hand back verbatim
// — today the images it describes. Indexing makes an image *searchable*
// (phases 1-3: its description is indexed as text); a blob store makes it
// *displayable* again, because the description alone cannot be shown to a
// user, the original file may have moved, and data URIs are stripped from the
// rendered chunks.
//
// Blobs are content-addressed: the hash of the content is the key, so storing
// the same bytes twice is a no-op and two documents embedding the same image
// share one entry. Documents reference them through a stable internal URI
// (see URI) that survives the markdown rendering.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/pkg/errors"
)

// Hash identifies a blob: the hex-encoded sha256 of its content.
type Hash string

// Info is the metadata of a stored blob.
type Info struct {
	Hash     Hash
	MimeType string
	Size     int64
}

var (
	// ErrNotFound is returned by Get when no blob carries the given hash.
	ErrNotFound = errors.New("blob not found")
	// ErrTooLarge is returned by Put when the content exceeds the store limit.
	ErrTooLarge = errors.New("blob too large")
)

// DefaultMaxBytes bounds the size of a single blob. It mirrors the default
// image size limit of the vision describer: content the model would refuse is
// not worth storing either.
const DefaultMaxBytes int64 = 10 << 20 // 10 MiB

// Store persists content addressed by the hash of its bytes.
//
// Implementations must be safe for concurrent use.
type Store interface {
	// Put stores the content and returns its hash. It is idempotent: putting
	// the same content again is a no-op returning the same hash.
	Put(ctx context.Context, mimeType string, data []byte) (Hash, error)
	// Get returns the content and metadata of a blob, or ErrNotFound.
	Get(ctx context.Context, hash Hash) ([]byte, *Info, error)
	// Delete removes the given blobs. Deleting an unknown hash is not an
	// error: the caller's intent (that the blob be gone) is satisfied.
	Delete(ctx context.Context, hashes ...Hash) error
	// List walks every stored blob. Returning an error from fn stops the walk
	// and is returned to the caller.
	List(ctx context.Context, fn func(Info) error) error
}

// ComputeHash returns the hash content will be stored under.
func ComputeHash(data []byte) Hash {
	sum := sha256.Sum256(data)

	return Hash(hex.EncodeToString(sum[:]))
}

// Valid reports whether h has the shape of a hash this package produces.
// Stores use it to reject a caller-supplied identifier before it reaches the
// filesystem or the database.
func (h Hash) Valid() bool {
	if len(h) != sha256.Size*2 {
		return false
	}

	_, err := hex.DecodeString(string(h))

	return err == nil
}

// Scheme and host of the internal blob URIs.
const (
	URIScheme = "amoxtli"
	URIHost   = "images"
)

// uriPrefix is the full prefix of an internal blob URI.
const uriPrefix = URIScheme + "://" + URIHost + "/"

// URI returns the internal URI referencing a blob, the form written into the
// markdown of indexed documents.
//
// The scheme is what makes it work end to end: markdown.StripDataURL only
// strips `data:` destinations, so `amoxtli://images/<hash>` survives the
// rendering of a chunk. An agent therefore sees the reference right next to
// the description text and knows what to ask for (see the fetch_image MCP
// tool).
func URI(hash Hash) string {
	return uriPrefix + string(hash)
}

// ParseURI extracts the hash from an internal blob URI. It also accepts a bare
// hash, so a tool can be lenient about what an agent passes back.
func ParseURI(raw string) (Hash, bool) {
	candidate := strings.TrimSpace(raw)

	if after, found := strings.CutPrefix(candidate, uriPrefix); found {
		candidate = after
	}

	hash := Hash(candidate)
	if !hash.Valid() {
		return "", false
	}

	return hash, true
}

// CheckPut validates what every implementation must refuse identically —
// empty content, missing media type, oversized payload — and returns the hash
// the content must be stored under. Implementations call it first thing in
// Put so the conformance suite sees the same behaviour everywhere.
func CheckPut(mimeType string, data []byte, maxBytes int64) (Hash, error) {
	if len(data) == 0 {
		return "", errors.New("blob: refusing to store empty content")
	}

	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	if int64(len(data)) > maxBytes {
		return "", errors.Wrapf(ErrTooLarge, "blob is %d bytes, limit is %d", len(data), maxBytes)
	}

	if mimeType == "" {
		return "", errors.New("blob: mime type is required")
	}

	return ComputeHash(data), nil
}

// ScanHashes extracts every blob hash referenced by an internal URI in the
// given content. It is how the garbage collector derives the live set: the
// documents themselves are the source of truth, with no reference table to
// keep in sync.
func ScanHashes(content []byte) []Hash {
	var hashes []Hash

	rest := content

	for {
		index := bytes.Index(rest, []byte(uriPrefix))
		if index < 0 {
			return hashes
		}

		rest = rest[index+len(uriPrefix):]

		// A hash is fixed-length hex: take exactly that many bytes and check
		// them, so trailing markdown syntax (")", "\n", ...) is ignored.
		if len(rest) < hashLength {
			return hashes
		}

		hash := Hash(rest[:hashLength])
		if hash.Valid() {
			hashes = append(hashes, hash)
			rest = rest[hashLength:]
		}
	}
}

// hashLength is the length of the hex encoding of a sha256 sum.
const hashLength = sha256.Size * 2
