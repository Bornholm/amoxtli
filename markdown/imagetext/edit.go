package imagetext

import "sort"

// edit is a replacement of the source range [start, end) by text. An insertion
// is the degenerate case start == end.
type edit struct {
	start, end int
	text       string
}

// applyEdits rebuilds data with every edit applied, in a single pass. Edits are
// given in any order; overlapping ones are dropped (the first in source order
// wins) so a malformed range can never scramble the document.
func applyEdits(data []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return data
	}

	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}

		// At equal position, a replacement comes before an insertion so that a
		// destination rewrite is not pushed inside the text inserted after it.
		return edits[i].end > edits[j].end
	})

	var (
		out    []byte
		cursor int
		total  int
	)

	for _, e := range edits {
		total += len(e.text)
	}

	out = make([]byte, 0, len(data)+total)

	for _, e := range edits {
		if e.start < cursor || e.start > len(data) || e.end > len(data) || e.start > e.end {
			continue
		}

		out = append(out, data[cursor:e.start]...)
		out = append(out, e.text...)
		cursor = e.end
	}

	return append(out, data[cursor:]...)
}
