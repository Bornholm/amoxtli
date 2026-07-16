package sourcecode

import (
	"slices"
	"testing"
)

func TestDefaultRegistryLookup(t *testing.T) {
	registry := DefaultRegistry()

	testCases := []struct {
		Extension string
		Language  string
	}{
		{Extension: ".go", Language: "go"},
		{Extension: ".GO", Language: "go"},
		{Extension: ".js", Language: "javascript"},
		{Extension: ".mjs", Language: "javascript"},
		{Extension: ".jsx", Language: "javascript"},
		{Extension: ".ts", Language: "typescript"},
		{Extension: ".tsx", Language: "tsx"},
		{Extension: ".py", Language: "python"},
		{Extension: ".php", Language: "php"},
	}

	for _, tc := range testCases {
		lang, exists := registry.Lookup(tc.Extension)
		if !exists {
			t.Errorf("Lookup(%q): expected a language, got none", tc.Extension)
			continue
		}

		if e, g := tc.Language, lang.Name; e != g {
			t.Errorf("Lookup(%q): expected language '%s', got '%s'", tc.Extension, e, g)
		}
	}

	if _, exists := registry.Lookup(".rb"); exists {
		t.Error("Lookup(\".rb\"): expected no language")
	}
}

func TestRegistryRegisterOverride(t *testing.T) {
	registry := DefaultRegistry()

	php, exists := ByName("php")
	if !exists {
		t.Fatal("ByName(\"php\"): expected a language")
	}

	registry.Register(".phtml", php)

	lang, exists := registry.Lookup(".phtml")
	if !exists {
		t.Fatal("Lookup(\".phtml\"): expected a language")
	}

	if e, g := "php", lang.Name; e != g {
		t.Errorf("Lookup(\".phtml\"): expected language '%s', got '%s'", e, g)
	}

	if !slices.Contains(registry.SupportedExtensions(), ".phtml") {
		t.Error("SupportedExtensions(): expected to contain '.phtml'")
	}
}

func TestNames(t *testing.T) {
	names := Names()

	for _, expected := range []string{"go", "javascript", "typescript", "tsx", "python", "php"} {
		if !slices.Contains(names, expected) {
			t.Errorf("Names(): expected to contain '%s' (got %v)", expected, names)
		}
	}
}
