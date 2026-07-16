package cli

import (
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// parseScalar interprets a raw flag value as bool, then number, then string,
// so that "amoxtli add --meta year=2024" and "amoxtli search --filter
// year>2020" agree on the stored type.
func parseScalar(raw string) any {
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}

	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}

	return raw
}

// parseMetadata parses repeated key=value flags into a metadata map.
func parseMetadata(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	metadata := map[string]any{}

	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, errors.Errorf("invalid metadata %q (expected key=value)", pair)
		}

		metadata[key] = parseScalar(value)
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

// parseFilters parses repeated key<op>value flags (=, !=, >, >=, <, <=) into
// metadata filter conditions.
func parseFilters(exprs []string) ([]index.Condition, error) {
	conditions := make([]index.Condition, 0, len(exprs))

	for _, expr := range exprs {
		matched := false

		for _, op := range filterOperators {
			key, value, ok := strings.Cut(expr, op.token)
			if !ok || key == "" {
				continue
			}

			conditions = append(conditions, op.build(key, parseScalar(value)))
			matched = true

			break
		}

		if !matched {
			return nil, errors.Errorf("invalid filter %q (expected key=value, key!=value, key>value, key>=value, key<value or key<=value)", expr)
		}
	}

	return conditions, nil
}
