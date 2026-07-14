// Package vec embeds a SQLite WASM build that includes the sqlite-vec extension,
// for use with the ncruces/go-sqlite3 driver.
//
// It is a vendored copy of the "ncruces" package from
// github.com/Bornholm/sqlite-vec-go-bindings (itself a fork of
// github.com/asg017/sqlite-vec-go-bindings), inlined here so that amoxtli
// remains installable with a plain `go get`, without a go.mod `replace`
// directive.
//
// Version constraints (both enforced by go.mod, breaking either one breaks the
// sqlite-vec backend):
//
//   - github.com/ncruces/go-sqlite3 must stay at v0.23.0: the embedded
//     sqlite3.wasm targets that host ABI (2-arg go_busy_timeout, Binary /
//     RuntimeConfig hooks). v0.17.1 and earlier fail to instantiate; v0.30.5+
//     expect a newer guest contract (go_final) this wasm does not provide, and
//     the newest releases dropped Binary / RuntimeConfig entirely.
//   - github.com/tetratelabs/wazero must stay at v1.9.0 or later: the wazero
//     v1.8.2 optimizing compiler (the version ncruces v0.23.0 pins by default)
//     mis-compiles sqlite-vec's vec0Filter, causing an out-of-bounds memory
//     access panic on every KNN query. v1.9.0+ fixes it; the interpreter
//     runtime is unaffected but far slower.
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
	Install()
}

// Install points ncruces/go-sqlite3 at the embedded sqlite-vec WASM build. It
// runs from init, but is exported so it can be re-applied explicitly: when the
// process also imports a plain ncruces build (e.g. the gorm SQLite store pulls
// github.com/ncruces/go-sqlite3/embed, which sets Binary to a vanilla SQLite in
// its own init), the two providers race on the global sqlite3.Binary and init
// order is undefined. Calling Install once, before the first connection is
// opened, deterministically selects the vec0-enabled build (a superset that
// serves the plain store just as well).
func Install() {
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
