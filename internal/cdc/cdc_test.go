package cdc

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// legislativeText renders articles as a single section would hold them: a title
// paragraph followed by one to three paragraphs of text.
func legislativeText(articles []string) string {
	return strings.Join(articles, "\n\n")
}

func article(r *rand.Rand, n int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Article L. %d-%d**", n, r.Intn(1000))
	for range 1 + r.Intn(3) {
		sb.WriteString("\n\n")
		words := 20 + r.Intn(100)
		for w := range words {
			if w > 0 {
				sb.WriteByte(' ')
			}
			fmt.Fprintf(&sb, "mot%d", r.Intn(5000))
		}
		sb.WriteByte('.')
	}
	return sb.String()
}

func TestSplitBoundsAndCoverage(t *testing.T) {
	r := rand.New(rand.NewSource(1))

	var articles []string
	for i := range 80 {
		articles = append(articles, article(r, i))
	}
	text := legislativeText(articles)

	const max = 3000

	chunks := Split(text, max, 200)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if len(c) > max {
			t.Errorf("chunk %d is %d bytes, above the %d bound", i, len(c), max)
		}
		if !strings.Contains(text, c) {
			t.Errorf("chunk %d is not a substring of the text", i)
		}
	}

	// Paragraph-bounded chunks do not overlap: together they hold the text.
	if got, want := len(strings.Join(chunks, "\n\n")), len(text); got != want {
		t.Errorf("chunks joined back hold %d bytes, want %d", got, want)
	}
}

func TestSplitIsStableAcrossLocalEdits(t *testing.T) {
	const max = 3000

	edits := map[string]func(articles []string, fresh string) []string{
		"insert at start": func(a []string, fresh string) []string {
			return append([]string{fresh}, a...)
		},
		"insert in middle": func(a []string, fresh string) []string {
			mid := len(a) / 2
			return append(append(append([]string{}, a[:mid]...), fresh), a[mid:]...)
		},
		"modify": func(a []string, fresh string) []string {
			b := append([]string{}, a...)
			b[len(b)/5] = fresh
			return b
		},
		"delete": func(a []string, _ string) []string {
			i := len(a) / 5
			return append(append([]string{}, a[:i]...), a[i+1:]...)
		},
	}

	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			var recomputed, total int

			for trial := range int64(20) {
				r := rand.New(rand.NewSource(trial))

				var articles []string
				for i := range 150 {
					articles = append(articles, article(r, i))
				}

				before := map[string]bool{}
				for _, c := range Split(legislativeText(articles), max, 200) {
					before[c] = true
				}

				after := Split(legislativeText(edit(articles, article(r, 9999))), max, 200)
				for _, c := range after {
					if !before[c] {
						recomputed++
					}
				}
				total += len(after)
			}

			// A local edit must only invalidate the chunks around it: with fixed
			// windows every chunk after the edit would change.
			if share := float64(recomputed) / float64(total); share > 0.08 {
				t.Errorf("%d of %d chunks recomputed (%.0f%%), want a local invalidation", recomputed, total, share*100)
			}
		})
	}
}

func TestSplitFallsBackToWindows(t *testing.T) {
	// A single paragraph without any newline, with multi-byte runes.
	text := strings.Repeat("délibération ", 1000)

	chunks := Split(text, 500, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected several windows, got %d", len(chunks))
	}

	for i, c := range chunks {
		if len(c) > 500 {
			t.Errorf("window %d is %d bytes, above the bound", i, len(c))
		}
		if !utf8.ValidString(c) {
			t.Errorf("window %d cuts a rune", i)
		}
	}
}

func TestSplitShortText(t *testing.T) {
	if got := Split("  court  ", 100, 10); len(got) != 1 || got[0] != "court" {
		t.Errorf("Split(short) = %q", got)
	}
	if got := Split(" \n\n ", 100, 10); len(got) != 0 {
		t.Errorf("Split(blank) = %q, want no chunk", got)
	}
}

func TestIsAnchorShare(t *testing.T) {
	anchors := 0
	for i := range 10000 {
		if IsAnchor(fmt.Sprintf("Article L. %d", i)) {
			anchors++
		}
	}
	// About one block in AnchorRatio, including on near-identical blocks.
	if anchors < 4500 || anchors > 5500 {
		t.Errorf("%d anchors out of 10000, want about half", anchors)
	}
}
