// Package lang centralizes natural language detection, so that the lexical
// analyzers (bleve, postgres) and the metadata enrichment applied at ingestion
// agree on what language a text is written in.
//
// Languages are always reported as ISO 639-1 codes ("fr", "en", ...).
package lang

import (
	"sort"
	"strings"

	"github.com/abadojack/whatlanggo"
)

// MaxDetected caps how many languages DetectAll reports for a single text.
const MaxDetected = 8

// minSegmentBytes is the length below which a segment is not submitted to the
// detector on its own. Trigram statistics need a sentence or so to settle; on
// anything shorter the verdict is a coin flip, and a wrong "reliable" is worse
// than no answer.
const minSegmentBytes = 60

// Detect returns the ISO 639-1 code of the dominant language of text, and
// whether the detection is reliable. Callers that record the result — as
// searchable metadata, say — must honour the boolean: whatlanggo always
// returns *a* language, and on short or code-heavy inputs that guess is
// arbitrary.
//
// Dominance is measured by volume of text, so a document that is mostly French
// with an English appendix reports French rather than being written off as
// undecidable.
func Detect(text string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return "", false
	}

	if weighted := detectBySegment(text); len(weighted) > 0 {
		return weighted[0].code, true
	}

	// No segment was long or distinctive enough for a verdict. whatlanggo
	// still has an opinion on the text as a whole; hand it over with its own
	// reliability flag rather than silently dropping the language.
	info := whatlanggo.Detect(text)

	return info.Lang.Iso6391(), info.IsReliable()
}

// DetectAll returns the ISO 639-1 codes of the languages found in text,
// ordered by decreasing volume of text and capped at max (MaxDetected when max
// is zero or negative).
//
// Detection runs per paragraph rather than on the whole text: a mixed document
// is a sequence of monolingual blocks, and each block is decidable on its own,
// whereas the blend of two languages is not — whatlanggo scores the union of
// French and English at 0.48 confidence and declares it unreliable, which is
// how a bilingual corpus ends up indexed under a single analyzer.
//
// When no paragraph is decidable, the result falls back to the single
// best-guess language of the whole text, so a short field still gets its
// analyzer instead of none.
func DetectAll(text string, max int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	if max <= 0 {
		max = MaxDetected
	}

	weighted := detectBySegment(text)
	if len(weighted) == 0 {
		info := whatlanggo.Detect(text)
		return []string{info.Lang.Iso6391()}
	}

	if len(weighted) > max {
		weighted = weighted[:max]
	}

	langs := make([]string, 0, len(weighted))
	for _, w := range weighted {
		langs = append(langs, w.code)
	}

	return langs
}

// weightedLang is a language and the number of bytes of text attributed to it.
type weightedLang struct {
	code  string
	bytes int
}

// detectBySegment splits text into paragraphs, detects each one separately and
// returns the reliably detected languages ordered by decreasing volume. Ties
// break on the language code, so the result is stable across runs.
func detectBySegment(text string) []weightedLang {
	byLang := map[string]int{}

	for _, segment := range segments(text) {
		info := whatlanggo.Detect(segment)
		if !info.IsReliable() {
			continue
		}

		byLang[info.Lang.Iso6391()] += len(segment)
	}

	weighted := make([]weightedLang, 0, len(byLang))
	for code, bytes := range byLang {
		weighted = append(weighted, weightedLang{code: code, bytes: bytes})
	}

	sort.Slice(weighted, func(i, j int) bool {
		if weighted[i].bytes != weighted[j].bytes {
			return weighted[i].bytes > weighted[j].bytes
		}

		return weighted[i].code < weighted[j].code
	})

	return weighted
}

// segments splits text on blank lines and merges the runts into their
// successor, so that a heading or a one-line paragraph is judged together with
// the text it introduces rather than on its own.
func segments(text string) []string {
	var (
		out     []string
		pending strings.Builder
	)

	flush := func() {
		if pending.Len() >= minSegmentBytes {
			out = append(out, pending.String())
			pending.Reset()
		}
	}

	for block := range strings.SplitSeq(text, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		if pending.Len() > 0 {
			pending.WriteString("\n")
		}

		pending.WriteString(block)

		flush()
	}

	// Whatever is left over is shorter than minSegmentBytes on its own, but
	// when it is all there is the caller still deserves an answer.
	if len(out) == 0 && pending.Len() > 0 {
		out = append(out, pending.String())
	}

	return out
}
