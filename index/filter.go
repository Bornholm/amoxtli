package index

import (
	"strings"
	"time"
)

// FilterOp enumerates the comparison operators supported by a metadata Filter.
type FilterOp string

const (
	// OpEq matches when the metadata value equals the condition value.
	OpEq FilterOp = "eq"
	// OpNe matches when the metadata value differs from the condition value
	// (including when the key is absent).
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
// Metadata typically originates from JSON, so numeric values surface as
// float64 and dates as RFC 3339 strings; the evaluator normalizes numeric
// types and recognizes time.Time / RFC 3339 strings for ordered comparisons.
// A condition on an absent key never matches, except OpNe which does.
type Filter []Condition

// Matches reports whether meta satisfies every condition. An empty filter
// always matches; a nil meta only satisfies OpNe conditions.
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

	switch c.Op {
	case OpEq:
		return present && equalValues(value, c.Value)
	case OpNe:
		// An absent key is, by definition, not equal to the target value.
		return !present || !equalValues(value, c.Value)
	case OpIn:
		if !present {
			return false
		}
		for _, candidate := range toSlice(c.Value) {
			if equalValues(value, candidate) {
				return true
			}
		}
		return false
	case OpGt, OpGte, OpLt, OpLte:
		if !present {
			return false
		}
		cmp, ok := compareValues(value, c.Value)
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

// equalValues compares two scalar values, normalizing numeric types so that an
// int condition matches a float64 metadata value (and vice versa).
func equalValues(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			return fa == fb
		}
		return false
	}
	if ta, ok := toTime(a); ok {
		if tb, ok := toTime(b); ok {
			return ta.Equal(tb)
		}
		return false
	}
	return a == b
}

// compareValues returns -1, 0 or 1 comparing a to b for ordered operators. The
// bool is false when the values are not mutually comparable.
func compareValues(a, b any) (int, bool) {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			switch {
			case fa < fb:
				return -1, true
			case fa > fb:
				return 1, true
			default:
				return 0, true
			}
		}
		return 0, false
	}

	if ta, ok := toTime(a); ok {
		if tb, ok := toTime(b); ok {
			return ta.Compare(tb), true
		}
		return 0, false
	}

	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return strings.Compare(sa, sb), true
		}
	}

	return 0, false
}

func toFloat(v any) (float64, bool) {
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

// toTime recognizes time.Time values and RFC 3339 formatted strings so that
// date metadata (usually JSON strings) can be range-filtered.
func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

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

// Ne builds an inequality condition (key != value).
func Ne(key string, value any) Condition { return Condition{Key: key, Op: OpNe, Value: value} }

// Gt builds a strictly-greater-than condition (key > value).
func Gt(key string, value any) Condition { return Condition{Key: key, Op: OpGt, Value: value} }

// Gte builds a greater-than-or-equal condition (key >= value).
func Gte(key string, value any) Condition { return Condition{Key: key, Op: OpGte, Value: value} }

// Lt builds a strictly-less-than condition (key < value).
func Lt(key string, value any) Condition { return Condition{Key: key, Op: OpLt, Value: value} }

// Lte builds a less-than-or-equal condition (key <= value).
func Lte(key string, value any) Condition { return Condition{Key: key, Op: OpLte, Value: value} }

// In builds a membership condition (key ∈ values).
func In(key string, values ...any) Condition {
	return Condition{Key: key, Op: OpIn, Value: values}
}
