package retrieval

import (
	"unicode"
	"unicode/utf8"
)

// Tokenizer splits text into the terms LexicalReranker scores on.
//
// Scan calls fn once per token, in order, with the token's position in the
// sequence. Positions may skip values — a tokenizer removing stop words leaves
// the gaps in place, which is what keeps the proximity feature measuring
// distance in the original text rather than in the filtered stream.
//
// The term slice is only valid for the duration of the call: implementations
// are free to reuse their buffer, and a caller keeping a term must copy it.
// That contract is what lets the default tokenizer walk a section without
// allocating a string per word — on a rerank window that would be tens of
// thousands of short-lived allocations, and it dominated the cost of the whole
// component before it was removed.
//
// It is an extension point for one concrete reason, not for symmetry: the
// default tokenizer splits on non-alphanumeric runes, which produces a single
// term for a whole Chinese or Japanese clause. A corpus in those languages
// needs a segmenter here, or the reranker silently scores every candidate the
// same while the bleve index handles them correctly through its cjk analyzer.
type Tokenizer interface {
	Scan(text string, fn func(position int, term []byte))
}

// SimpleTokenizer lowercases and splits on anything that is not a letter or a
// digit. It has no stemming, no stop-word list and no language awareness.
//
// That sounds like a weakness next to the per-language analyzers the bleve
// index uses, and it was measured not to be one.
//
// Swapping in those analyzers moves nDCG@10 by +0.003 on two English datasets
// and by −0.036 on French PIAF, for 58x the tokenization cost (24µs → 1.4ms per
// section, language detection dominating). Stemming trades precision for
// recall, and the reranker is on the wrong side of that trade: it widens what a
// *search* finds, but the candidates are already retrieved by the time the
// reranker runs, and collapsing distinct surface forms only removes the
// discriminating power it needs to order them. French suffers most, bleve's
// French analyzer stemming aggressively while removing no stop words.
//
// What it does *not* handle is a script without word separators: a Chinese or
// Japanese sentence comes out as one term. Those corpora need a segmenter
// supplied through WithTokenizer.
type SimpleTokenizer struct{}

// Scan implements Tokenizer.
func (SimpleTokenizer) Scan(text string, fn func(position int, term []byte)) {
	var buf []byte
	position := 0

	flush := func() {
		if len(buf) == 0 {
			return
		}
		fn(position, buf)
		position++
		buf = buf[:0]
	}

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf = utf8.AppendRune(buf, unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
}

var _ Tokenizer = SimpleTokenizer{}
