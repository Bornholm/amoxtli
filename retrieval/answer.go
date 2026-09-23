package retrieval

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// Default budgets of an answer prompt. An answer needs more of each section
// than a relevance judgment does.
const (
	DefaultAnswerMaxSources      = 8
	DefaultAnswerMaxSectionWords = 400
	DefaultAnswerMaxTotalWords   = 6000
)

const answerSystemPrompt = `You answer questions using only the numbered sources given by the user.

Rules:
- Rely exclusively on the sources. Never use outside knowledge, never guess.
- Cite every statement with the number of its source in square brackets, e.g. [1] or [2][3], right after the statement.
- Cite only the numbers of the given sources.
- If the sources do not contain the answer, or only part of it, say so plainly instead of completing it.
- Answer in the language of the question, concisely, in Markdown.`

// AnswerSource is a section given to the answer model, numbered as the model
// cites it.
type AnswerSource struct {
	Number    int
	SectionID model.SectionID
	Source    *index.SearchResult
}

// AnswerDelta is a piece of a streamed answer, or the error that ended it.
type AnswerDelta struct {
	Text string
	Err  error
}

// AnswerOption configures an answer.
type AnswerOption func(*answerOptions)

type answerOptions struct {
	maxSources      int
	maxSectionWords int
	maxTotalWords   int
}

// WithAnswerMaxSources bounds the number of sections given to the model.
func WithAnswerMaxSources(n int) AnswerOption {
	return func(o *answerOptions) {
		if n > 0 {
			o.maxSources = n
		}
	}
}

// WithAnswerMaxSectionWords bounds the words of each section in the prompt.
func WithAnswerMaxSectionWords(n int) AnswerOption {
	return func(o *answerOptions) {
		if n > 0 {
			o.maxSectionWords = n
		}
	}
}

// WithAnswerMaxTotalWords bounds the words of all the sections in the
// prompt.
func WithAnswerMaxTotalWords(n int) AnswerOption {
	return func(o *answerOptions) {
		if n > 0 {
			o.maxTotalWords = n
		}
	}
}

// Answerer writes an answer to a query from retrieved sections, citing them.
type Answerer struct {
	llm   llm.Client
	store SectionStore
}

// NewAnswerer builds an answerer.
func NewAnswerer(client llm.Client, store SectionStore) *Answerer {
	return &Answerer{llm: client, store: store}
}

// Answer numbers the sections of the results, best first, and streams the
// model's answer. The returned sources are those the numbers refer to; the
// channel is closed when the answer is complete.
func (a *Answerer) Answer(ctx context.Context, query string, results []*index.SearchResult, funcs ...AnswerOption) ([]AnswerSource, <-chan AnswerDelta, error) {
	opts := answerOptions{
		maxSources:      DefaultAnswerMaxSources,
		maxSectionWords: DefaultAnswerMaxSectionWords,
		maxTotalWords:   DefaultAnswerMaxTotalWords,
	}
	for _, fn := range funcs {
		fn(&opts)
	}

	sections, err := a.store.GetSectionsByIDs(ctx, allSectionIDs(results))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	var (
		sources    []AnswerSource
		prompt     strings.Builder
		totalWords int
	)

	prompt.WriteString("## Question\n\n")
	prompt.WriteString(query)
	prompt.WriteString("\n\n## Sources\n\n")

	for _, result := range results {
		for _, id := range result.Sections {
			if len(sources) >= opts.maxSources || totalWords >= opts.maxTotalWords {
				break
			}
			section, ok := sections[id]
			if !ok {
				continue
			}
			content, err := section.Content()
			if err != nil {
				return nil, nil, errors.WithStack(err)
			}
			text, consumed := truncateSection(string(content), opts.maxSectionWords, opts.maxTotalWords-totalWords)
			if consumed == 0 {
				continue
			}
			totalWords += consumed

			source := AnswerSource{Number: len(sources) + 1, SectionID: id, Source: result}
			sources = append(sources, source)

			prompt.WriteString("### [")
			prompt.WriteString(strconv.Itoa(source.Number))
			prompt.WriteString("]")
			if result.Source != nil {
				prompt.WriteString(" ")
				prompt.WriteString(result.Source.String())
			}
			prompt.WriteString("\n\n")
			prompt.WriteString(text)
			prompt.WriteString("\n\n")
		}
	}

	if len(sources) == 0 {
		return nil, nil, errors.WithStack(ErrNoSources)
	}

	stream, err := a.llm.ChatCompletionStream(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, answerSystemPrompt),
			llm.NewMessage(llm.RoleUser, prompt.String()),
		),
		llm.WithTemperature(0.1),
	)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	deltas := make(chan AnswerDelta)
	go func() {
		defer close(deltas)
		for chunk := range stream {
			var delta AnswerDelta
			switch chunk.Type() {
			case llm.StreamChunkTypeError:
				delta.Err = chunk.Error()
			case llm.StreamChunkTypeDelta:
				if chunk.Delta() == nil || chunk.Delta().Content() == "" {
					continue
				}
				delta.Text = chunk.Delta().Content()
			default:
				continue
			}
			select {
			case deltas <- delta:
			case <-ctx.Done():
				return
			}
			if delta.Err != nil {
				return
			}
		}
	}()

	return sources, deltas, nil
}

// ErrNoSources reports an answer asked without any section to rely on.
var ErrNoSources = errors.New("no section to answer from")

var citation = regexp.MustCompile(`\[(\d{1,3})\]`)

// Citations returns the source numbers cited by an answer, in order of first
// citation, ignoring the numbers outside 1..sources.
func Citations(answer string, sources int) []int {
	var cited []int
	for _, m := range citation.FindAllStringSubmatch(answer, -1) {
		n, _ := strconv.Atoi(m[1])
		if n >= 1 && n <= sources && !slices.Contains(cited, n) {
			cited = append(cited, n)
		}
	}
	return cited
}
