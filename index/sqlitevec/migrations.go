package sqlitevec

import (
	"fmt"

	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// DefaultVectorSize is the default dimension of the vec0 embedding column,
// matching common 768-dimension embedding models.
const DefaultVectorSize int = 768

// schemaVersion is the current on-disk schema version, recorded under the
// amoxtli_meta "schema_version" key.
//
// v2 partitions the vec0 table by collection (one row per chunk × collection)
// so KNN queries prune to the filtered collection instead of scanning the whole
// corpus and filtering afterwards — which both sped up filtered searches and
// silently returned fewer than k results (the global top-k could live in other
// collections).
//
// v3 adds document_metadata, the copy of each document's metadata this index
// needs to evaluate a metadata filter inside its own KNN query
// (index.FilterableIndex). Upgrading is a no-op on existing data: the table
// starts empty and documents fill it as they are (re)indexed.
const schemaVersion = "3"

// supportsCoarse reports whether the vector dimension allows the binary
// quantization column (sqlite-vec requires a multiple of 8).
func supportsCoarse(vectorSize int) bool {
	return vectorSize%8 == 0
}

// migrations returns the version-independent schema statements (metadata
// tables); the versioned vec0 schema is handled by upgradeSchema.
func migrations() []string {
	return []string{
		// Main embeddings table (chunk metadata only)
		`
			CREATE TABLE IF NOT EXISTS embeddings (
				id INTEGER NOT NULL PRIMARY KEY,
				source TEXT,
				section_id TEXT,
				chunk_index INTEGER DEFAULT 0
			);
		`,
		`CREATE INDEX IF NOT EXISTS embeddings_lookup_idx ON embeddings (section_id);`,
		"CREATE INDEX IF NOT EXISTS embeddings_source_idx ON embeddings ( source );",
		"CREATE TABLE IF NOT EXISTS embeddings_collections ( embeddings_id INTEGER, collection_id TEXT NOT NULL, FOREIGN KEY (embeddings_id) REFERENCES embeddings (id) ON DELETE CASCADE );",
		"CREATE INDEX IF NOT EXISTS embeddings_collections_idx ON embeddings_collections ( embeddings_id, collection_id );",
		// Identity metadata (embeddings model + vector size + schema version),
		// used to reject opening an existing index with an incompatible
		// configuration and to drive schema upgrades.
		"CREATE TABLE IF NOT EXISTS amoxtli_meta ( key TEXT PRIMARY KEY, value TEXT NOT NULL );",
		// Per-document metadata (schema v3), keyed by source like the
		// embeddings rows. It is this index's own copy of what the store holds:
		// pushing a metadata filter into the KNN query requires the values to
		// be readable from here. A document indexed before v3 — or carrying no
		// metadata — simply has no row, which the filter translation reads as
		// an empty object.
		`
			CREATE TABLE IF NOT EXISTS document_metadata (
				source TEXT NOT NULL PRIMARY KEY,
				metadata TEXT NOT NULL
			);
		`,
	}
}

// v2Statements returns the schema of the current (v2) vector storage:
//
//   - embeddings_vec_map assigns a stable rowid to each (chunk, collection)
//     pair — the vec0 rowid — and keeps the vec-side rows queryable by
//     embeddings_id with a regular index (vec0 auxiliary columns cannot be
//     filtered on);
//   - embeddings_vec2 holds one vector row per (chunk, collection), with the
//     collection as vec0 partition key so a filtered KNN scans only that
//     collection's rows. Chunks without a collection use the ” partition.
//     When the dimension is a multiple of 8 an embedding_coarse bit column
//     stores the binary-quantized vector for the optional two-stage
//     (Hamming then float re-scoring) search.
func v2Statements(vectorSize int) []string {
	vecColumns := fmt.Sprintf("embedding float[%d]", vectorSize)
	if supportsCoarse(vectorSize) {
		vecColumns += fmt.Sprintf(", embedding_coarse bit[%d]", vectorSize)
	}

	return []string{
		`
			CREATE TABLE IF NOT EXISTS embeddings_vec_map (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				embeddings_id INTEGER NOT NULL,
				collection_id TEXT NOT NULL DEFAULT ''
			);
		`,
		`CREATE INDEX IF NOT EXISTS embeddings_vec_map_embeddings_idx ON embeddings_vec_map (embeddings_id);`,
		fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS embeddings_vec2 USING vec0(%s, collection_id text partition key);", vecColumns),
	}
}

// upgradeSchema brings the vector storage to the current schema version. A
// fresh database gets the v2 schema directly; a v1 database (single
// unpartitioned embeddings_vec table) is migrated in place: the existing
// vector blobs are copied — not recomputed — into the partitioned table, one
// row per (chunk, collection), then the old table is dropped.
func upgradeSchema(conn *sqlite3.Conn, vectorSize int) error {
	version, err := getMetadata(conn, "schema_version")
	if err != nil {
		return errors.WithStack(err)
	}

	if version == schemaVersion {
		// Idempotent: (re)create the v2 tables if missing.
		for _, sql := range v2Statements(vectorSize) {
			if err := conn.Exec(sql); err != nil {
				return errors.Wrapf(err, "could not execute migration '%s'", sql)
			}
		}
		return nil
	}

	hasV1, err := tableExists(conn, "embeddings_vec")
	if err != nil {
		return errors.WithStack(err)
	}

	for _, sql := range v2Statements(vectorSize) {
		if err := conn.Exec(sql); err != nil {
			return errors.Wrapf(err, "could not execute migration '%s'", sql)
		}
	}

	if hasV1 {
		// One map row per (chunk, collection); chunks without any collection
		// land in the '' partition.
		copyStatements := []string{
			`
				INSERT INTO embeddings_vec_map (embeddings_id, collection_id)
				SELECT e.id, ec.collection_id
				FROM embeddings e
				JOIN embeddings_collections ec ON ec.embeddings_id = e.id;
			`,
			`
				INSERT INTO embeddings_vec_map (embeddings_id, collection_id)
				SELECT e.id, ''
				FROM embeddings e
				WHERE NOT EXISTS (SELECT 1 FROM embeddings_collections ec WHERE ec.embeddings_id = e.id);
			`,
		}

		coarseExpr := ""
		coarseColumn := ""
		if supportsCoarse(vectorSize) {
			coarseColumn = ", embedding_coarse"
			coarseExpr = ", vec_quantize_binary(v.embedding)"
		}

		copyStatements = append(copyStatements, fmt.Sprintf(`
				INSERT INTO embeddings_vec2 (rowid, embedding%s, collection_id)
				SELECT m.id, v.embedding%s, m.collection_id
				FROM embeddings_vec_map m
				JOIN embeddings_vec v ON v.rowid = m.embeddings_id;
			`, coarseColumn, coarseExpr),
			"DROP TABLE embeddings_vec;",
		)

		for _, sql := range copyStatements {
			if err := conn.Exec(sql); err != nil {
				return errors.Wrapf(err, "could not migrate vector schema to v%s: '%s'", schemaVersion, sql)
			}
		}
	}

	if err := setMetadata(conn, "schema_version", schemaVersion); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// tableExists reports whether a (regular or virtual) table exists.
func tableExists(conn *sqlite3.Conn, name string) (bool, error) {
	stmt, _, err := conn.Prepare("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?;")
	if err != nil {
		return false, errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, name); err != nil {
		return false, errors.WithStack(err)
	}

	exists := stmt.Step()
	if err := stmt.Err(); err != nil {
		return false, errors.WithStack(err)
	}

	return exists, nil
}

// getMetadata reads one amoxtli_meta value; a missing key returns "".
func getMetadata(conn *sqlite3.Conn, key string) (string, error) {
	stmt, _, err := conn.Prepare("SELECT value FROM amoxtli_meta WHERE key = ?;")
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, key); err != nil {
		return "", errors.WithStack(err)
	}

	value := ""
	if stmt.Step() {
		value = stmt.ColumnText(0)
	}
	if err := stmt.Err(); err != nil {
		return "", errors.WithStack(err)
	}

	return value, nil
}
