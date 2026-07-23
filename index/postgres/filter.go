package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/filternorm"
	"github.com/jackc/pgx/v5"
	"github.com/pkg/errors"
)

// metadataJoin brings each chunk's document metadata into a search query. It is
// a LEFT JOIN so that documents without a metadata row still reach the filter,
// where they read as "no key present": dm.metadata is then SQL NULL, every
// json operator on it yields NULL, and a NULL predicate is not true — exactly
// the required semantics, for free.
//
// Keeping the conditions on dm.metadata rather than on a COALESCE'd expression
// also leaves the GIN index usable for containment.
const metadataJoin = ` LEFT JOIN amoxtli_document_metadata dm ON dm.source = c.source`

// binaryCollation forces byte-per-byte text comparison.
//
// PostgreSQL compares text with the database collation, which for a typical
// en_US.UTF-8 database ignores punctuation at the primary level — so
// '2026-07-13...' and '20260713...' can compare equal, and a non-deterministic
// ICU collation can even make 'a' = 'A'. The Go evaluator compares bytes. The
// "C" collation is what makes the two agree, and it is what makes lexicographic
// order on canonical dates chronological.
const binaryCollation = ` COLLATE "C"`

// writeDocumentMetadata stores this index's copy of a document's metadata,
// canonicalized with the very same function the store and the Go evaluator use
// — dates especially, since they are compared as text here.
func writeDocumentMetadata(ctx context.Context, tx pgx.Tx, source string, metadata map[string]any) error {
	if len(metadata) == 0 {
		// Nothing to store: the absent row already means "no key present".
		return nil
	}

	encoded, err := json.Marshal(filternorm.Metadata(metadata))
	if err != nil {
		return errors.WithStack(err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO amoxtli_document_metadata (source, metadata) VALUES ($1, $2)
		ON CONFLICT (source) DO UPDATE SET metadata = EXCLUDED.metadata;
	`, source, string(encoded))

	return errors.WithStack(err)
}

// args accumulates bind parameters and hands out their positional placeholders.
// Keys and values are always bound, never interpolated: metadata keys reach us
// from caller-facing surfaces.
type args struct {
	values []any
}

func (a *args) bind(value any) string {
	a.values = append(a.values, value)

	return "$" + strconv.Itoa(len(a.values))
}

// buildFilterSQL translates a filter into a boolean SQL expression over
// dm.metadata, appending its bind parameters to a.
//
// It reproduces the semantics specified on index.Filter, and is validated
// against the same conformance suite as the Go evaluator (index/filtertest).
func buildFilterSQL(filter index.Filter, a *args) (string, error) {
	if len(filter) == 0 {
		return "TRUE", nil
	}

	if err := filter.Validate(); err != nil {
		return "", errors.WithStack(err)
	}

	clauses := make([]string, 0, len(filter))
	for _, condition := range filter {
		clauses = append(clauses, buildCondition(condition, a))
	}

	return strings.Join(clauses, " AND "), nil
}

// alwaysFalse translates a condition no document can satisfy, which is how
// operand kinds that cannot be compared yield no match instead of an error.
const alwaysFalse = "FALSE"

func buildCondition(condition index.Condition, a *args) string {
	switch condition.Op {
	case index.OpExists:
		want, ok := condition.Value.(bool)
		if !ok {
			return alwaysFalse
		}
		if want {
			return exists(condition.Key, a)
		}
		// The only operator a document without a metadata row can satisfy,
		// hence the explicit NULL branch.
		return "(dm.metadata IS NULL OR NOT " + exists(condition.Key, a) + ")"

	case index.OpEq:
		return equals(condition.Key, condition.Value, a)

	case index.OpNe:
		// Presence is required: Ne does not match a document lacking the key.
		// The guard also keeps the expression from evaluating to NULL, which
		// WHERE would reject anyway but for the wrong reason.
		return "(" + exists(condition.Key, a) + " AND NOT (" + equals(condition.Key, condition.Value, a) + "))"

	case index.OpIn:
		elements := index.InValues(condition.Value)
		if len(elements) == 0 {
			return alwaysFalse
		}

		clauses := make([]string, 0, len(elements))
		for _, element := range elements {
			clauses = append(clauses, equals(condition.Key, element, a))
		}

		return "(" + strings.Join(clauses, " OR ") + ")"

	case index.OpGt:
		return ordered(condition.Key, condition.Value, ">", a)
	case index.OpGte:
		return ordered(condition.Key, condition.Value, ">=", a)
	case index.OpLt:
		return ordered(condition.Key, condition.Value, "<", a)
	case index.OpLte:
		return ordered(condition.Key, condition.Value, "<=", a)
	}

	return alwaysFalse
}

// exists tests key presence. jsonb_exists is true for a key explicitly set to
// JSON null, which counts as present under these semantics.
func exists(key string, a *args) string {
	return "jsonb_exists(dm.metadata, " + a.bind(key) + ")"
}

// equals translates an equality test, guarding it with the JSON type the
// operand requires so mismatched kinds never match.
//
// Note what is deliberately absent: a cast of the stored value. PostgreSQL does
// not guarantee it evaluates the AND operands in written order, so a
// (dm.metadata->>k)::numeric would be free to run before the jsonb_typeof guard
// meant to protect it and raise an error where the semantics demand "no match".
// Comparing jsonb to jsonb instead moves the cast onto the bind parameter,
// which is ours and always well-typed.
func equals(key string, operand any, a *args) string {
	switch value := filternorm.Value(operand).(type) {
	case bool:
		return "(jsonb_typeof(dm.metadata -> " + a.bind(key) + ") = 'boolean'" +
			" AND dm.metadata -> " + a.bind(key) + " = to_jsonb(" + a.bind(value) + "::boolean))"

	case string:
		return "(jsonb_typeof(dm.metadata -> " + a.bind(key) + ") = 'string'" +
			" AND (dm.metadata ->> " + a.bind(key) + ")" + binaryCollation + " = " + a.bind(value) + binaryCollation + ")"

	default:
		number, ok := filternorm.Float(value)
		if !ok {
			// nil, slices, maps: present maybe, equal never.
			return alwaysFalse
		}
		return "(jsonb_typeof(dm.metadata -> " + a.bind(key) + ") = 'number'" +
			" AND dm.metadata -> " + a.bind(key) + " = to_jsonb(" + a.bind(number) + "::numeric))"
	}
}

// ordered translates a range comparison. Only numbers and strings are ordered;
// booleans, null and containers are not, matching the Go evaluator.
func ordered(key string, operand any, op string, a *args) string {
	switch value := filternorm.Value(operand).(type) {
	case bool:
		return alwaysFalse

	case string:
		return "(jsonb_typeof(dm.metadata -> " + a.bind(key) + ") = 'string'" +
			" AND (dm.metadata ->> " + a.bind(key) + ")" + binaryCollation + " " + op + " " + a.bind(value) + binaryCollation + ")"

	default:
		number, ok := filternorm.Float(value)
		if !ok {
			return alwaysFalse
		}
		// jsonb ordering between two numbers is numeric ordering, so the guard
		// above is what makes this comparison meaningful.
		return "(jsonb_typeof(dm.metadata -> " + a.bind(key) + ") = 'number'" +
			" AND dm.metadata -> " + a.bind(key) + " " + op + " to_jsonb(" + a.bind(number) + "::numeric))"
	}
}
