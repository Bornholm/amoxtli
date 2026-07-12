// Package vec embeds a SQLite WASM build that includes the sqlite-vec extension,
// for use with the ncruces/go-sqlite3 driver.
//
// It is a vendored copy of the "ncruces" package from
// github.com/Bornholm/sqlite-vec-go-bindings (itself a fork of
// github.com/asg017/sqlite-vec-go-bindings), inlined here so that amoxtli
// remains installable with a plain `go get`, without a go.mod `replace`
// directive. The embedded sqlite3.wasm is built against a recent
// ncruces/go-sqlite3 release; update it in lockstep with that dependency.
//
// See LICENSE in this directory for the sqlite-vec license and attribution.
package vec

import (
	"bytes"
	_ "embed"
	"encoding/binary"

	"github.com/ncruces/go-sqlite3"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

//go:embed sqlite3.wasm
var wasmBinary []byte

func init() {
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
	sqlite3.Binary = wasmBinary
}

// SerializeFloat32 serializes a float32 slice into a vector BLOB that
// sqlite-vec accepts.
func SerializeFloat32(vector []float32) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, vector)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
