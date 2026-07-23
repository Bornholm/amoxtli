package cli

import (
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bornholm/amoxtli/internal/filternorm"
)

// docMetadata is what "add" and "sync" attach to each document they index: the
// pairs given on the command line and, unless --no-file-metadata is set, a set
// of attributes derived from the file itself.
//
// The derived keys are the ones a caller would otherwise have to pass by hand
// on every invocation, and they are the ones "amoxtli search --filter" can act
// on: narrowing a search to a subtree (dirname), to a file type (extension), or
// to recently touched documents (mtime).
type docMetadata struct {
	// user holds the --meta pairs. They win over every derived value.
	user map[string]any
	// file enables the derived attributes.
	file bool
}

// Keys of the derived file attributes. All match index.KeyPattern, so each one
// is directly usable as a search filter key.
const (
	// metaFilename is the file's base name, extension included ("guide.md").
	metaFilename = "filename"
	// metaExtension is the lowercase extension without its leading dot ("md").
	metaExtension = "extension"
	// metaSize is the file size in bytes.
	metaSize = "size"
	// metaMTime is the file's last modification time on disk.
	metaMTime = "mtime"
	// metaDirname is the directory holding the file, expressed like the stored
	// source: relative to --base-dir when set, absolute otherwise.
	metaDirname = "dirname"
	// metaIndexedAt is when the document was handed to the indexer. Unlike
	// mtime it describes the index, not the disk, and answers "what has not
	// been refreshed since X".
	metaIndexedAt = "indexed_at"
)

// build assembles the metadata map for one file. abs is its absolute path,
// source the URL it is indexed under — dirname is derived from the latter so it
// never discloses more of the host's layout than the stored source already
// does.
//
// Dates are written in the canonical layout rather than as time.Time values:
// the metadata map is serialized to JSON on its way through the task queue,
// where the Go type would be lost anyway, and canonical text is what makes
// ordered comparisons on dates work identically in every backend.
//
// Returns nil when there is nothing to attach, so the caller can skip the
// option entirely.
func (m docMetadata) build(abs string, info os.FileInfo, source *url.URL) map[string]any {
	if !m.file {
		return m.user
	}

	metadata := map[string]any{
		metaFilename:  filepath.Base(abs),
		metaExtension: strings.TrimPrefix(strings.ToLower(filepath.Ext(abs)), "."),
		metaSize:      info.Size(),
		metaMTime:     filternorm.FormatTime(info.ModTime()),
		metaDirname:   path.Dir(source.Path),
		metaIndexedAt: filternorm.FormatTime(time.Now()),
	}

	// --meta is the escape hatch when a derived value is wrong or unwanted, so
	// it is applied last.
	maps.Copy(metadata, m.user)

	return metadata
}
