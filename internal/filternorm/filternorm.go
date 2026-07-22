// Package filternorm holds the value normalization and comparison rules that
// define the semantics of index.Filter.
//
// It exists to be the single source of truth shared by every implementation of
// those semantics: the Go evaluator, the SQL push-down translators of the
// filterable backends, and the write path that canonicalizes stored metadata.
// The invariant these implementations must preserve is
//
//	matchGo(f, metadata) == matchSQL(f, metadata)
//
// for every filter and every document. A divergence would make the same query
// return different documents depending on the index backend — silently. Any
// change to the rules below therefore belongs here, never in a caller.
package filternorm

import (
	"cmp"
	"strings"
	"time"
)

// CanonicalTimeLayout is the representation of every time value stored in
// document metadata: RFC 3339, UTC, fixed nanosecond precision.
//
// Fixing both the zone and the precision is what makes lexicographic order
// equal chronological order — "2024-01-02T10:00:00+02:00" sorts after
// "2024-01-02T09:00:00Z" as text while being earlier in time. Once every stored
// date is canonical, a SQL backend can range-filter dates with a plain text
// comparison, which is the only way the push-down of ordered operators on dates
// can be made reliable.
const CanonicalTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t in the canonical layout.
func FormatTime(t time.Time) string {
	return t.UTC().Format(CanonicalTimeLayout)
}

// Value canonicalizes a scalar so that both sides of a comparison — the stored
// metadata and the filter operand — share one representation. time.Time values
// and RFC 3339 strings collapse to CanonicalTimeLayout; every other value is
// returned unchanged. It is idempotent, so applying it to already-stored
// (already-canonical) metadata is a no-op.
func Value(v any) any {
	switch t := v.(type) {
	case time.Time:
		return FormatTime(t)
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return FormatTime(parsed)
		}
		return t
	default:
		return v
	}
}

// Metadata returns a copy of m with every value canonicalized by Value. It is
// applied on the write path so that what is persisted is directly comparable by
// both the Go evaluator and the SQL translators.
//
// Nested values (slices, maps) are copied as-is: filters do not reach into
// containers, so there is nothing to canonicalize there.
func Metadata(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}

	normalized := make(map[string]any, len(m))
	for k, v := range m {
		normalized[k] = Value(v)
	}

	return normalized
}

// Float reports the numeric value of v, unifying every Go numeric type into
// float64. JSON decodes all numbers as float64, so an int written by a Go
// caller must compare equal to the float64 read back from the store.
func Float(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// Equal reports whether a and b are equal under filter semantics: numbers
// compare numerically across Go types, strings (including canonicalized dates)
// compare byte-exactly, booleans compare by value. Any other pairing — mixed
// types, nil, slices, maps — is not equal rather than an error, so that the
// evaluation of a filter is total.
func Equal(a, b any) bool {
	a, b = Value(a), Value(b)

	if fa, ok := Float(a); ok {
		fb, ok := Float(b)
		return ok && fa == fb
	}

	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}

	if ba, ok := a.(bool); ok {
		bb, ok := b.(bool)
		return ok && ba == bb
	}

	return false
}

// Compare returns -1, 0 or 1 ordering a against b for the ordered operators.
// Only numbers (numerically) and strings (lexicographically, which is
// chronologically for canonical dates) are ordered; the bool is false for every
// other pairing, including booleans and containers.
func Compare(a, b any) (int, bool) {
	a, b = Value(a), Value(b)

	if fa, ok := Float(a); ok {
		fb, ok := Float(b)
		if !ok {
			return 0, false
		}
		return cmp.Compare(fa, fb), true
	}

	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(sa, sb), true
	}

	return 0, false
}
