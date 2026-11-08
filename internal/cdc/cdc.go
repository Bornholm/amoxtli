// Package cdc implements content-defined chunking: a text is cut at blocks
// chosen by a hash of their own content rather than at fixed offsets, so an
// edit only moves the cuts around it.
//
// Packing blocks greedily up to a size bound makes every cut depend on all the
// blocks before it: inserting one paragraph at the top of a long section shifts
// every following chunk, and each shifted chunk is a new embeddings input. With
// content-defined cuts, two versions of a text that differ locally go back to
// cutting at the same anchor blocks right after the edit, and the unchanged
// chunks keep their exact text — hence their embeddings cache key.
package cdc

import (
	"hash/fnv"
	"strings"
	"unicode"
)

// AnchorRatio is the inverse of the share of blocks that are anchors. One block
// in two gave the best stability on legislative texts (articles of one to
// three paragraphs) while keeping chunks close to their size bound.
const AnchorRatio = 2

// IsAnchor reports whether a block is a content-defined cut point. It depends
// on the block's content only (surrounding whitespace ignored), never on its
// position, and is stable across processes.
func IsAnchor(block string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.TrimSpace(block)))
	// FNV-1a alone is biased on near-identical blocks ("Article L. 12" / "…13"
	// differ in their last byte only): the splitmix64 finalizer spreads that
	// difference over every bit.
	return mix(h.Sum64())%AnchorRatio == 0
}

// mix is the splitmix64 finalizer.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Policy decides where a sequence of blocks is cut, sizes being measured in
// any unit (words, bytes) as long as it is consistent.
type Policy struct {
	// Min is the size a chunk must reach before an anchor may close it.
	Min int
	// Max is the size bound of a chunk.
	Max int
}

// NewPolicy returns the policy used for a size bound: anchors may cut from
// half of it.
func NewPolicy(max int) Policy {
	return Policy{Min: max / 2, Max: max}
}

// CutBefore reports whether the chunk holding size units, previous being its
// last block, must be closed before block, of blockSize units. Either block is
// an anchor (see Anchored) and the chunk reached Min, or block is a lead (see
// IsLead) that would bring the chunk to Max: it then opens the next chunk
// rather than ending this one, away from the text it introduces.
func (p Policy) CutBefore(size, blockSize int, previous, block string) bool {
	if size <= 0 {
		return false
	}

	if IsLead(block) && size+blockSize >= p.Max {
		return true
	}

	return size >= p.Min && Anchored(previous, block)
}

// Anchored reports whether a cut may fall between previous and block: block is
// an anchor, and previous is not a lead block — a heading-like line such as an
// article title stays with the text it introduces. Cutting before an anchor
// rather than after it also opens each chunk on an anchor.
func Anchored(previous, block string) bool {
	return !IsLead(previous) && IsAnchor(block)
}

// LeadWords is the word count up to which a block is a lead (see IsLead).
const LeadWords = 12

// IsLead reports whether a block is short enough to be a heading-like line
// introducing the next one ("Article L. 12", "Section 2 : Dispositions
// générales").
func IsLead(block string) bool {
	words := 0
	for range strings.FieldsSeq(block) {
		words++
		if words > LeadWords {
			return false
		}
	}
	return true
}

// Split cuts text into chunks of at most max bytes, each an exact substring of
// text with surrounding whitespace trimmed. Cuts fall on paragraph boundaries
// (blank lines) when possible, then on line boundaries; a single line longer
// than max is cut into windows of max bytes overlapping by overlap bytes, at
// rune boundaries.
func Split(text string, max, overlap int) []string {
	if max <= 0 || len(text) <= max {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return []string{trimmed}
		}
		return nil
	}

	units := segment(text, 0, len(text), max, overlap, []string{"\n\n", "\n"})
	policy := NewPolicy(max)

	var chunks []string

	emit := func(start, end int) {
		if chunk := strings.TrimSpace(text[start:end]); chunk != "" {
			chunks = append(chunks, chunk)
		}
	}

	// previous is the last unit of the current chunk, last the end of the
	// unit before it.
	var previous span
	start, end, last := -1, 0, 0
	for _, u := range units {
		if start >= 0 {
			unit := text[u.start:u.end]

			switch {
			// Never exceed max: the chunk also closes when this unit would not
			// fit anymore — before its lead unit when it ends on one, so that a
			// title never ends a chunk away from its text.
			case u.end-start > max:
				if previous.start > start && u.end-previous.start <= max && IsLead(text[previous.start:previous.end]) {
					emit(start, last)
					start = previous.start
				} else {
					emit(start, end)
					start = -1
				}

			case policy.CutBefore(end-start, u.end-u.start, text[previous.start:previous.end], unit):
				emit(start, end)
				start = -1
			}
		}

		if start < 0 {
			start = u.start
		}
		last, end = end, u.end
		previous = u
	}

	if start >= 0 {
		emit(start, end)
	}

	return chunks
}

type span struct{ start, end int }

// segment splits text[start:end] on the first separator, recursing with the
// next ones into the segments still longer than max, and falls back to
// overlapping windows when no separator is left.
func segment(text string, start, end, max, overlap int, separators []string) []span {
	if end-start <= max {
		if strings.TrimSpace(text[start:end]) == "" {
			return nil
		}
		return []span{{start, end}}
	}

	if len(separators) == 0 {
		return windows(text, start, end, max, overlap)
	}

	sep := separators[0]

	var spans []span
	for from := start; from < end; {
		to := strings.Index(text[from:end], sep)
		if to < 0 {
			to = end
		} else {
			to += from
		}

		spans = append(spans, segment(text, from, to, max, overlap, separators[1:])...)

		from = to + len(sep)
	}

	return spans
}

// windows cuts text[start:end] into windows of at most max bytes overlapping by
// overlap bytes, moving the bounds back to rune starts and, when one is close
// enough, to a space.
func windows(text string, start, end, max, overlap int) []span {
	overlap = min(overlap, max/2)

	var spans []span
	for from := start; from < end; {
		to := min(from+max, end)
		if to < end {
			to = runeStart(text, to)
			if space := strings.LastIndexFunc(text[from:to], unicode.IsSpace); space > (to-from)/2 {
				to = from + space
			}
		}

		spans = append(spans, span{from, to})

		if to == end {
			break
		}

		next := runeStart(text, to-overlap)
		if next <= from {
			next = to
		}
		from = next
	}

	return spans
}

// runeStart moves i back to the start of the rune it falls into.
func runeStart(text string, i int) int {
	for i > 0 && i < len(text) && text[i]&0xC0 == 0x80 {
		i--
	}
	return i
}
