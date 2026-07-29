package lang

import (
	"slices"
	"strings"
	"testing"
)

const (
	frenchText = `La recherche documentaire hybride combine un canal lexical et un canal
sémantique. Le premier retrouve les termes exacts de la requête, le second
rapproche les formulations différentes d'une même idée. Leur fusion donne un
rappel supérieur à celui de chacun pris isolément.`

	englishText = `Hybrid document retrieval combines a lexical channel and a semantic one.
The former recovers the exact terms of the query, the latter brings together
different phrasings of the same idea. Fusing them yields a recall higher than
either taken on its own.`
)

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name         string
		text         string
		wantCode     string
		wantReliable bool
	}{
		{name: "French", text: frenchText, wantCode: "fr", wantReliable: true},
		{name: "English", text: englishText, wantCode: "en", wantReliable: true},
		{name: "Empty", text: "", wantCode: "", wantReliable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, reliable := Detect(tc.text)

			if reliable != tc.wantReliable {
				t.Errorf("Detect(%q) reliable: got %v, want %v", tc.name, reliable, tc.wantReliable)
			}

			// The code of an unreliable detection is meaningless, only the
			// flag is part of the contract.
			if tc.wantReliable && code != tc.wantCode {
				t.Errorf("Detect(%q) code: got %q, want %q", tc.name, code, tc.wantCode)
			}
		})
	}
}

// TestDetectShortTextIsUnreliable pins the reason Detect returns a boolean at
// all: a query-sized input is not enough for a verdict, and callers recording
// the result must not treat the guess as fact.
func TestDetectShortTextIsUnreliable(t *testing.T) {
	if _, reliable := Detect("hello"); reliable {
		t.Error("Detect(\"hello\"): got reliable, want unreliable")
	}
}

func TestDetectAll(t *testing.T) {
	t.Run("MixedText", func(t *testing.T) {
		langs := DetectAll(frenchText+"\n\n"+englishText, 0)

		for _, want := range []string{"fr", "en"} {
			if !slices.Contains(langs, want) {
				t.Errorf("DetectAll(mixed) = %v, want it to contain %q", langs, want)
			}
		}
	})

	// Detecting on the whole text scores a French/English blend at ~0.48
	// confidence — "unreliable" — and reports neither language. Per-paragraph
	// detection is what keeps a bilingual document from falling back to a
	// single default analyzer.
	t.Run("MixedTextDoesNotCollapseToUnreliable", func(t *testing.T) {
		langs := DetectAll(frenchText+"\n\n"+englishText, 0)

		if len(langs) != 2 {
			t.Errorf("DetectAll(mixed) = %v, want exactly the two languages present", langs)
		}
	})

	t.Run("Empty", func(t *testing.T) {
		if langs := DetectAll("", 0); langs != nil {
			t.Errorf("DetectAll(\"\") = %v, want nil", langs)
		}
	})

	// The dominant language is the one with the most text, not the one that
	// happens to come first.
	t.Run("OrderedByVolume", func(t *testing.T) {
		langs := DetectAll(englishText+"\n\n"+strings.Repeat(frenchText+"\n\n", 3), 0)

		if len(langs) == 0 || langs[0] != "fr" {
			t.Errorf("DetectAll(mostly fr) = %v, want French first", langs)
		}
	})

	// A short field is below the per-segment threshold, but the caller still
	// needs an analyzer for it.
	t.Run("ShortTextStillYieldsAGuess", func(t *testing.T) {
		if langs := DetectAll("Bonjour le monde", 0); len(langs) != 1 {
			t.Errorf("DetectAll(short) = %v, want a single best guess", langs)
		}
	})

	t.Run("HonoursMax", func(t *testing.T) {
		// A text mixing enough languages that the iterative walk keeps
		// finding reliable results past the cap.
		text := strings.Join([]string{frenchText, englishText}, "\n\n")

		if langs := DetectAll(text, 1); len(langs) != 1 {
			t.Errorf("DetectAll(mixed, 1) = %v, want a single language", langs)
		}
	})

	t.Run("DefaultsToMaxDetected", func(t *testing.T) {
		if langs := DetectAll(frenchText, -1); len(langs) > MaxDetected {
			t.Errorf("DetectAll(fr, -1) returned %d languages, want at most %d", len(langs), MaxDetected)
		}
	})
}
