package index

import (
	"regexp"

	"github.com/bornholm/amoxtli/internal/filternorm"
	"github.com/pkg/errors"
)

// FilterOp enumerates the comparison operators supported by a metadata Filter.
type FilterOp string

const (
	// OpEq matches when the metadata value equals the condition value.
	OpEq FilterOp = "eq"
	// OpNe matches when the key is present and its value differs from the
	// condition value. An absent key does NOT match (see Filter).
	OpNe FilterOp = "ne"
	// OpGt matches when the metadata value is strictly greater than the value.
	OpGt FilterOp = "gt"
	// OpGte matches when the metadata value is greater than or equal to the value.
	OpGte FilterOp = "gte"
	// OpLt matches when the metadata value is strictly less than the value.
	OpLt FilterOp = "lt"
	// OpLte matches when the metadata value is less than or equal to the value.
	OpLte FilterOp = "lte"
	// OpIn matches when the metadata value equals any element of the value slice.
	OpIn FilterOp = "in"
	// OpExists matches on key presence only, whatever the value. The condition
	// value is a bool: true requires the key, false requires its absence.
	OpExists FilterOp = "exists"
)

// Condition is a single predicate over one metadata key.
type Condition struct {
	Key   string
	Op    FilterOp
	Value any
}

// Filter is a conjunction (implicit AND) of conditions evaluated against a
// document's metadata. The zero value (nil / empty) matches everything.
//
// # Semantics
//
// These rules are normative: they are the contract every implementation must
// honour, whether the filter is evaluated in Go over metadata loaded from the
// store or pushed down into a backend's search query. The shared conformance
// suite in index/filtertest encodes them case by case.
//
// Evaluation is total: a condition never errors at match time, it matches or it
// does not.
//
//   - Key presence. A key is present when it appears in the metadata map,
//     whatever its value — including a JSON null. Every operator except
//     OpExists requires presence, OpNe included: Ne("author", "x") does not
//     match a document without an "author" key. This is the SQL NULL-like
//     reading, chosen because it is the one a SQL backend can express
//     faithfully; use NotExists to select documents lacking a key.
//
//   - Numbers. All Go numeric types are unified into float64, on both sides and
//     for every operator, so Eq("count", 3) matches the float64(3) that JSON
//     decoding produces.
//
//   - Dates. time.Time values and RFC 3339 strings are canonicalized to UTC
//     with fixed nanosecond precision (filternorm.CanonicalTimeLayout), on the
//     write path and on the filter operand alike. Equality is then an exact
//     string comparison and ordering a lexicographic one, which for canonical
//     dates is chronological order.
//
//   - Strings. Compared exactly: case-sensitive, accent-sensitive. The
//     unaccenting applied to full-text search does not extend to metadata.
//
//   - Mixed types. Comparing values of different kinds — Gt("count", "abc")
//     against a numeric metadata, a bool against a string — never matches.
//
//   - Containers. A metadata value that is a slice or a map is present but
//     never equal nor ordered, so Eq("tags", "go") does not match
//     tags: ["go", "db"]. Membership in an array is deliberately left out of
//     this version; it would need an explicit Contains operator.
//
//   - Empty In. In(key) without any value matches nothing, like SQL IN ().
//
// # Keys
//
// Filter keys are restricted to KeyPattern. Metadata keys usually come from a
// caller-facing surface (CLI flags, the MCP search tool), and the restriction
// keeps them safely expressible as a JSON path in every backend. Validate
// reports offending keys as ErrInvalidFilterKey.
type Filter []Condition

// KeyPattern is the set of accepted metadata keys in a filter condition.
var KeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ErrInvalidFilterKey is returned by Filter.Validate for a key outside
// KeyPattern. Wrapped errors match it with errors.Is.
var ErrInvalidFilterKey = errors.New("invalid filter key")

// ValidateKey reports whether key is usable as a metadata filter key.
func ValidateKey(key string) error {
	if !KeyPattern.MatchString(key) {
		return errors.Wrapf(ErrInvalidFilterKey, "%q (expected %s)", key, KeyPattern)
	}

	return nil
}

// Validate checks every condition's key against KeyPattern. It is called by the
// search pipeline before a filter is evaluated or translated to SQL; callers
// building filters from user input can call it earlier to report the problem at
// its source.
func (f Filter) Validate() error {
	for _, c := range f {
		if err := ValidateKey(c.Key); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// Matches reports whether meta satisfies every condition. An empty filter
// always matches; a nil meta has no key present, so it only satisfies
// NotExists conditions.
func (f Filter) Matches(meta map[string]any) bool {
	for _, c := range f {
		if !c.matches(meta) {
			return false
		}
	}

	return true
}

func (c Condition) matches(meta map[string]any) bool {
	value, present := meta[c.Key]

	if c.Op == OpExists {
		want, ok := c.Value.(bool)
		return ok && want == present
	}

	if !present {
		return false
	}

	switch c.Op {
	case OpEq:
		return filternorm.Equal(value, c.Value)

	case OpNe:
		return !filternorm.Equal(value, c.Value)

	case OpIn:
		for _, candidate := range toSlice(c.Value) {
			if filternorm.Equal(value, candidate) {
				return true
			}
		}
		return false

	case OpGt, OpGte, OpLt, OpLte:
		cmp, ok := filternorm.Compare(value, c.Value)
		if !ok {
			return false
		}

		switch c.Op {
		case OpGt:
			return cmp > 0
		case OpGte:
			return cmp >= 0
		case OpLt:
			return cmp < 0
		case OpLte:
			return cmp <= 0
		}
	}

	return false
}

// toSlice normalizes the operand of OpIn. A non-slice value is treated as a
// single candidate so that a hand-built Condition{Op: OpIn, Value: "x"} behaves
// like In(key, "x").
func toSlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	default:
		return []any{v}
	}
}

// Eq builds an equality condition (key == value).
func Eq(key string, value any) Condition { return Condition{Key: key, Op: OpEq, Value: value} }

// Ne builds an inequality condition (key present and key != value). It does not
// match documents lacking the key; combine with Or-less semantics in mind, or
// use NotExists for absence.
func Ne(key string, value any) Condition { return Condition{Key: key, Op: OpNe, Value: value} }

// Gt builds a strictly-greater-than condition (key > value).
func Gt(key string, value any) Condition { return Condition{Key: key, Op: OpGt, Value: value} }

// Gte builds a greater-than-or-equal condition (key >= value).
func Gte(key string, value any) Condition { return Condition{Key: key, Op: OpGte, Value: value} }

// Lt builds a strictly-less-than condition (key < value).
func Lt(key string, value any) Condition { return Condition{Key: key, Op: OpLt, Value: value} }

// Lte builds a less-than-or-equal condition (key <= value).
func Lte(key string, value any) Condition { return Condition{Key: key, Op: OpLte, Value: value} }

// In builds a membership condition (key ∈ values). Without values it matches
// nothing.
func In(key string, values ...any) Condition {
	return Condition{Key: key, Op: OpIn, Value: values}
}

// Exists builds a presence condition: the document carries the key, whatever
// its value.
func Exists(key string) Condition { return Condition{Key: key, Op: OpExists, Value: true} }

// NotExists builds an absence condition: the document does not carry the key.
// It is the way to select documents missing a metadata, since Ne requires
// presence.
func NotExists(key string) Condition { return Condition{Key: key, Op: OpExists, Value: false} }
