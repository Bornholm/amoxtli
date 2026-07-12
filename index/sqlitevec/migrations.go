package sqlitevec

import "fmt"

// DefaultVectorSize is the default dimension of the vec0 embedding column,
// matching common 768-dimension embedding models.
const DefaultVectorSize int = 768

// migrations returns the schema statements for an index storing embeddings of
// the given dimension.
func migrations(vectorSize int) []string {
	return []string{
		// Main embeddings table (metadata only)
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
		// Identity metadata (embeddings model + vector size), used to reject
		// opening an existing index with an incompatible configuration.
		"CREATE TABLE IF NOT EXISTS amoxtli_meta ( key TEXT PRIMARY KEY, value TEXT NOT NULL );",
		// vec0 virtual table - vector only, metadata in embeddings table
		fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS embeddings_vec USING vec0(embedding float[%d]);", vectorSize),
	}
}
