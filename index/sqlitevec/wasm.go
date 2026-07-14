package sqlitevec

import vec "github.com/bornholm/amoxtli/index/sqlitevec/internal/vec"

// EnsureVecWASM forces ncruces/go-sqlite3 to load the sqlite-vec-enabled WASM
// build, overriding any plain SQLite build that another package may have
// selected.
//
// It is only needed when the same process opens a sqlite-vec index AND another
// ncruces/go-sqlite3 connection whose package imports the vanilla WASM build —
// most notably the gorm SQLite store (gorm.NewSQLiteStore), which transitively
// imports github.com/ncruces/go-sqlite3/embed. Both builds assign the global
// sqlite3.Binary from their init, so which one wins depends on undefined init
// order; without this call the sqlite-vec build may lose and vec0 virtual
// tables fail with "no such module: vec0".
//
// Call it once at startup, before opening ANY sqlite connection (the store
// included). The vec0-enabled build is a superset of plain SQLite, so the store
// works on it unchanged. It is unnecessary when sqlite-vec is the only
// ncruces/go-sqlite3 user in the process.
func EnsureVecWASM() {
	vec.Install()
}
