package sqlitevec

import (
	"encoding/json"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/filternorm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// metadataJSON is the JSON document a filter is evaluated against. A document
// with no metadata row has no metadata at all, which the semantics of
// index.Filter read as "no key is present" — exactly what an empty object
// yields, so COALESCE covers both the never-indexed and the metadata-less case
// without a special branch.
const metadataJSON = `COALESCE(dm.metadata, '{}')`

// jsonPath is the SQL expression building the JSON path of a filter key. The
// key is always a bind parameter, never interpolated: keys reach us from
// caller-facing surfaces (CLI flags, MCP arguments). index.Filter restricts them
// to [A-Za-z0-9_-], which also guarantees '$.' || key is a syntactically valid
// JSON1 path — an invalid path would raise a SQL error where the semantics
// demand a plain "no match".
const jsonPath = `('$.' || ?)`

// writeDocumentMetadata stores this index's copy of a document's metadata.
//
// The values are canonicalized with the very same function the store and the Go
// evaluator use, because the SQL translation compares dates as text: an
// un-normalized "2024-01-02T10:00:00+02:00" would sort after
// "2024-01-02T09:00:00Z" while being earlier in time.
func writeDocumentMetadata(conn *sqlite3.Conn, source string, metadata map[string]any) error {
	if len(metadata) == 0 {
		// Nothing to store: the absent row already means "no key present".
		return nil
	}

	encoded, err := json.Marshal(filternorm.Metadata(metadata))
	if err != nil {
		return errors.WithStack(err)
	}

	stmt, _, err := conn.Prepare(`
		INSERT INTO document_metadata (source, metadata) VALUES (?, ?)
		ON CONFLICT (source) DO UPDATE SET metadata = excluded.metadata;
	`)
	if err != nil {
		return errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, source); err != nil {
		return errors.WithStack(err)
	}
	if err := stmt.BindText(2, string(encoded)); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(stmt.Exec())
}

func deleteDocumentMetadata(conn *sqlite3.Conn, source string) error {
	stmt, _, err := conn.Prepare("DELETE FROM document_metadata WHERE source = ?;")
	if err != nil {
		return errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, source); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(stmt.Exec())
}

// rowidPrefilter returns the SQL fragment restricting a KNN to the vector rows
// of documents satisfying the filter, plus its bind arguments in order.
//
// vec0 accepts a rowid IN (...) constraint alongside its MATCH/k constraint and
// applies it *during* the scan. That is what makes this a real push-down: the k
// nearest neighbours are picked among the eligible rows, so a selective filter
// returns k results instead of decimating a top-k chosen without it.
func rowidPrefilter(filter index.Filter) (string, []any, error) {
	if len(filter) == 0 {
		return "", nil, nil
	}

	clause, args, err := buildFilterSQL(filter)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	return `
		AND v.rowid IN (
			SELECT m.id
			FROM embeddings_vec_map m
			JOIN embeddings e ON e.id = m.embeddings_id
			LEFT JOIN document_metadata dm ON dm.source = e.source
			WHERE ` + clause + `
		)`, args, nil
}

// buildFilterSQL translates a filter into a boolean SQL expression over
// metadataJSON, plus the bind arguments it expects in order.
//
// It reproduces the semantics specified on index.Filter, and is validated
// against the same conformance suite as the Go evaluator (index/filtertest).
// Every cast is guarded by a json_type check: without the guard, comparing a
// text value as a number raises a SQL error where the semantics require the
// condition to simply not match.
func buildFilterSQL(filter index.Filter) (string, []any, error) {
	if len(filter) == 0 {
		return "1", nil, nil
	}

	if err := filter.Validate(); err != nil {
		return "", nil, errors.WithStack(err)
	}

	clauses := make([]string, 0, len(filter))
	args := make([]any, 0, len(filter)*3)

	for _, condition := range filter {
		clause, conditionArgs := buildCondition(condition)
		clauses = append(clauses, clause)
		args = append(args, conditionArgs...)
	}

	return strings.Join(clauses, " AND "), args, nil
}

// alwaysFalse is the translation of a condition no document can satisfy. It is
// how the "evaluation is total" rule is honoured: operand kinds that cannot be
// compared yield no match instead of an error.
const alwaysFalse = "0"

func buildCondition(condition index.Condition) (string, []any) {
	key := condition.Key

	// json_type(...) IS NOT NULL is the presence test: it returns 'null' — not
	// SQL NULL — for a key explicitly set to JSON null, which is present under
	// these semantics.
	present := "json_type(" + metadataJSON + ", " + jsonPath + ") IS NOT NULL"

	switch condition.Op {
	case index.OpExists:
		want, ok := condition.Value.(bool)
		if !ok {
			return alwaysFalse, nil
		}
		if want {
			return present, []any{key}
		}
		return "json_type(" + metadataJSON + ", " + jsonPath + ") IS NULL", []any{key}

	case index.OpEq:
		return equals(key, condition.Value)

	case index.OpNe:
		// Presence is required: Ne does not match a document lacking the key.
		clause, args := equals(key, condition.Value)
		return "(" + present + " AND NOT (" + clause + "))", append([]any{key}, args...)

	case index.OpIn:
		elements := index.InValues(condition.Value)
		if len(elements) == 0 {
			return alwaysFalse, nil
		}

		clauses := make([]string, 0, len(elements))
		args := make([]any, 0, len(elements)*2)
		for _, element := range elements {
			clause, elementArgs := equals(key, element)
			clauses = append(clauses, clause)
			args = append(args, elementArgs...)
		}

		return "(" + strings.Join(clauses, " OR ") + ")", args

	case index.OpGt:
		return ordered(key, condition.Value, ">")
	case index.OpGte:
		return ordered(key, condition.Value, ">=")
	case index.OpLt:
		return ordered(key, condition.Value, "<")
	case index.OpLte:
		return ordered(key, condition.Value, "<=")
	}

	return alwaysFalse, nil
}

// equals translates an equality test, guarding the comparison with the JSON
// type the operand requires so that mismatched kinds never match.
func equals(key string, operand any) (string, []any) {
	extract := "json_extract(" + metadataJSON + ", " + jsonPath + ")"
	typeOf := "json_type(" + metadataJSON + ", " + jsonPath + ")"

	switch value := filternorm.Value(operand).(type) {
	case bool:
		// json_extract turns JSON booleans into 0/1, indistinguishable from
		// numbers; json_type keeps them apart.
		want := "false"
		if value {
			want = "true"
		}
		return "(" + typeOf + " = ?)", []any{key, want}

	case string:
		return "(" + typeOf + " = 'text' AND " + extract + " = ?)", []any{key, key, value}

	default:
		number, ok := filternorm.Float(value)
		if !ok {
			// nil, slices, maps: present maybe, equal never.
			return alwaysFalse, nil
		}
		return "(" + typeOf + " IN ('integer','real') AND " + extract + " = ?)", []any{key, key, number}
	}
}

// ordered translates a range comparison. Only numbers and strings are ordered;
// booleans, null and containers are not, matching the Go evaluator.
func ordered(key string, operand any, op string) (string, []any) {
	extract := "json_extract(" + metadataJSON + ", " + jsonPath + ")"
	typeOf := "json_type(" + metadataJSON + ", " + jsonPath + ")"

	switch value := filternorm.Value(operand).(type) {
	case bool:
		return alwaysFalse, nil

	case string:
		// SQLite's default BINARY collation compares text byte by byte, like
		// the Go evaluator — and, on canonical dates, chronologically.
		return "(" + typeOf + " = 'text' AND " + extract + " " + op + " ?)", []any{key, key, value}

	default:
		number, ok := filternorm.Float(value)
		if !ok {
			return alwaysFalse, nil
		}
		return "(" + typeOf + " IN ('integer','real') AND " + extract + " " + op + " ?)", []any{key, key, number}
	}
}

// bindArgs binds the arguments produced by buildFilterSQL, starting at the
// given 1-based parameter index and returning the next free one.
func bindArgs(stmt *sqlite3.Stmt, start int, args []any) (int, error) {
	position := start

	for _, arg := range args {
		var err error

		switch value := arg.(type) {
		case string:
			err = stmt.BindText(position, value)
		case float64:
			err = stmt.BindFloat(position, value)
		case int:
			err = stmt.BindInt(position, value)
		case bool:
			err = stmt.BindBool(position, value)
		case nil:
			err = stmt.BindNull(position)
		default:
			return position, errors.Errorf("cannot bind filter argument of type %T", arg)
		}

		if err != nil {
			return position, errors.WithStack(err)
		}

		position++
	}

	return position, nil
}
