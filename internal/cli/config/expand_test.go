package config

import (
	"strings"
	"testing"
)

func TestExpandEnv(t *testing.T) {
	env := map[string]string{
		"FOO":   "foo-value",
		"EMPTY": "",
	}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}

	cases := []struct {
		name     string
		input    string
		expected string
		wantErr  string
	}{
		{name: "braced", input: "key: ${FOO}", expected: "key: foo-value"},
		{name: "bare", input: "key: $FOO", expected: "key: foo-value"},
		{name: "default used", input: "key: ${MISSING:-fallback}", expected: "key: fallback"},
		{name: "default ignored when set", input: "key: ${FOO:-fallback}", expected: "key: foo-value"},
		{name: "empty value is defined", input: "key: ${EMPTY:-fallback}", expected: "key: "},
		{name: "empty default", input: "key: ${MISSING:-}", expected: "key: "},
		{name: "missing without default", input: "key: ${MISSING}", wantErr: "MISSING"},
		{name: "literal dollar", input: "cost: $$5", expected: "cost: $5"},
		{name: "full-line comment untouched", input: "# api_key: ${MISSING}\nkey: ${FOO}", expected: "# api_key: ${MISSING}\nkey: foo-value"},
		{name: "indented comment untouched", input: "  # ${MISSING}", expected: "  # ${MISSING}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ExpandEnv(tc.input, lookup)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got result %q", tc.wantErr, result)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %+v", err)
			}
			if result != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, result)
			}
		})
	}
}
