package filterexpr

import (
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// ParseScalar interprets a raw flag value as bool, then number, then string,
// so that "amoxtli add --meta year=2024" and "amoxtli search --filter
// year>2020" agree on the stored type.
// Only the spelled-out booleans are recognized: strconv.ParseBool also accepts
// "1", "0", "t" and "f", which would turn "--filter n>1" into a comparison
// against true and silently match nothing.
func ParseScalar(raw string) any {
	switch strings.ToLower(raw) {
	case "true":
		return true
	case "false":
		return false
	}

	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}

	return raw
}

// ParseMetadata parses repeated key=value flags into a metadata map.
func ParseMetadata(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	metadata := map[string]any{}

	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, errors.Errorf("invalid metadata %q (expected key=value)", pair)
		}

		metadata[key] = ParseScalar(value)
	}

	return metadata, nil
}

// filterOperators maps flag syntax to metadata filter conditions, longest
// operators first so that ">=" is not parsed as ">".
var filterOperators = []struct {
	token string
	build func(key string, value any) index.Condition
}{
	{token: "!=", build: func(k string, v any) index.Condition { return index.Ne(k, v) }},
	{token: ">=", build: func(k string, v any) index.Condition { return index.Gte(k, v) }},
	{token: "<=", build: func(k string, v any) index.Condition { return index.Lte(k, v) }},
	{token: "=", build: func(k string, v any) index.Condition { return index.Eq(k, v) }},
	{token: ">", build: func(k string, v any) index.Condition { return index.Gt(k, v) }},
	{token: "<", build: func(k string, v any) index.Condition { return index.Lt(k, v) }},
}

// ParseFilters parses repeated filter expressions into metadata filter
// conditions. Two forms are accepted:
//
//   - key<op>value with op in =, !=, >, >=, <, <=;
//   - key? (the document carries the key) and !key (it does not). Since key!=x
//     requires the key to be present, !key is the way to select documents
//     missing a metadata.
func ParseFilters(exprs []string) ([]index.Condition, error) {
	conditions := make([]index.Condition, 0, len(exprs))

	for _, expr := range exprs {
		if condition, ok, err := parsePresence(expr); err != nil {
			return nil, errors.WithStack(err)
		} else if ok {
			conditions = append(conditions, condition)
			continue
		}

		matched := false

		for _, op := range filterOperators {
			key, value, ok := strings.Cut(expr, op.token)
			if !ok || key == "" {
				continue
			}

			if err := index.ValidateKey(key); err != nil {
				return nil, errors.Wrapf(err, "invalid filter %q", expr)
			}

			conditions = append(conditions, op.build(key, ParseScalar(value)))
			matched = true

			break
		}

		if !matched {
			return nil, errors.Errorf("invalid filter %q (expected key=value, key!=value, key>value, key>=value, key<value, key<=value, key? or !key)", expr)
		}
	}

	return conditions, nil
}

// parsePresence recognizes the two presence forms, "key?" and "!key". The
// boolean reports whether expr is one of them; a malformed key is an error
// rather than a fallthrough to the comparison grammar, which could not parse it
// either.
func parsePresence(expr string) (index.Condition, bool, error) {
	var (
		key   string
		build func(string) index.Condition
	)

	switch {
	case strings.HasPrefix(expr, "!") && !strings.Contains(expr, "="):
		key, build = strings.TrimPrefix(expr, "!"), index.NotExists
	case strings.HasSuffix(expr, "?"):
		key, build = strings.TrimSuffix(expr, "?"), index.Exists
	default:
		return index.Condition{}, false, nil
	}

	if err := index.ValidateKey(key); err != nil {
		return index.Condition{}, false, errors.Wrapf(err, "invalid filter %q", expr)
	}

	return build(key), true, nil
}
