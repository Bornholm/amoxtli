package text

import (
	"unicode"
)

type Word struct {
	Start int
	End   int
}

func SplitByWords(text string) []*Word {
	words := make([]*Word, 0)

	var word *Word
	for idx, rune := range text {
		if unicode.IsSpace(rune) || unicode.IsPunct(rune) {
			if word != nil {
				word.End = idx
				words = append(words, word)
				word = nil
			}
		} else if word == nil {
			word = &Word{
				Start: idx,
			}
		}
	}

	if word != nil {
		word.End = len(text) - 1
		words = append(words, word)
	}

	return words
}

// Truncate truncates the given text to a maximum number of words
func Truncate(str string, maxWords int) string {
	words := SplitByWords(str)

	if len(words) <= maxWords {
		return str
	}

	strippingStart := words[0].Start
	strippingEnd := words[maxWords].End

	truncated := str[strippingStart:strippingEnd]

	return truncated
}

// MiddleOut truncates the given text by applying a "middle out" strategy
//
// See https://openrouter.ai/docs/features/message-transforms
// and https://arxiv.org/abs/2307.03172
func MiddleOut(str string, maxWords int, ellipsis string) string {
	words := SplitByWords(str)

	totalWords := len(words)

	if len(words) <= maxWords {
		return str
	}

	halvedDiff := (totalWords - maxWords) / 2
	middle := totalWords / 2

	strippingStart := words[middle-halvedDiff].Start
	strippingEnd := words[middle+halvedDiff].End

	truncated := str[:strippingStart] + ellipsis + str[strippingEnd:]

	return truncated
}
