package sourcecode

import (
	"bytes"
	"slices"

	corpusText "github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
)

// splitOversized walks the section tree and slices every leaf section whose
// content exceeds the word budget into sequential child sections, cutting at
// line boundaries. This mirrors the markdown parser force-split so oversized
// code files (or declaration bodies) stay fetchable at a sane granularity.
func splitOversized(document *Document, section *Section, maxWords int) {
	if maxWords <= 0 {
		return
	}

	for _, child := range section.sections {
		splitOversized(document, child, maxWords)
	}

	if len(section.sections) > 0 {
		return
	}

	content := document.data[section.start:section.end]

	words := corpusText.SplitByWords(string(content))
	if len(words) <= maxWords {
		return
	}

	cursor := 0

	for chunkStart := 0; chunkStart < len(words); chunkStart += maxWords {
		var cut int

		if chunkStart+maxWords >= len(words) {
			cut = len(content)
		} else if offset := bytes.IndexByte(content[words[chunkStart+maxWords-1].End:], '\n'); offset >= 0 {
			cut = words[chunkStart+maxWords-1].End + offset + 1
		} else {
			cut = len(content)
		}

		if cut <= cursor {
			continue
		}

		child := &Section{
			id:       model.NewSectionID(),
			document: document,
			parent:   section,
			level:    section.level + 1,
			sections: make([]*Section, 0),
			start:    section.start + cursor,
			end:      section.start + cut,
		}

		child.branch = append(slices.Clone(section.branch), child.id)

		section.sections = append(section.sections, child)

		cursor = cut

		if cursor >= len(content) {
			break
		}
	}
}
