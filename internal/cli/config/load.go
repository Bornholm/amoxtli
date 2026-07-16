package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Load reads, expands and validates the configuration file at path. Variable
// references are resolved against the process environment, then against an
// optional .env file sitting next to the configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	dotEnv, err := loadDotEnv(filepath.Join(filepath.Dir(path), ".env"))
	if err != nil {
		return nil, err
	}

	lookup := func(name string) (string, bool) {
		if value, ok := os.LookupEnv(name); ok {
			return value, true
		}

		value, ok := dotEnv[name]

		return value, ok
	}

	cfg, err := Parse(string(raw), lookup)
	if err != nil {
		return nil, errors.Wrapf(err, "could not load %s", path)
	}

	return cfg, nil
}

// Parse expands and decodes a raw configuration document, then validates it.
// Unknown fields are rejected to catch typos early.
func Parse(raw string, lookup func(string) (string, bool)) (*Config, error) {
	expanded, err := ExpandEnv(raw, lookup)
	if err != nil {
		return nil, err
	}

	cfg := Default()

	decoder := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	decoder.KnownFields(true)

	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.WithStack(err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// loadDotEnv parses a minimal KEY=VALUE file (blank lines and # comments
// ignored, optional "export " prefix, optional single or double quotes). A
// missing file is not an error.
func loadDotEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, errors.WithStack(err)
	}

	values := map[string]string{}

	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		values[key] = value
	}

	return values, nil
}
