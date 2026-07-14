package eval_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/eval"
	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/prompt"
	"github.com/pkg/errors"
)

// readerPrompt turns the retrieved passages into a short extractive answer. It
// deliberately asks for the shortest possible span so the generated answer is
// comparable to the reference answers under SQuAD-style EM/F1 (a chatty answer
// tanks EM and dilutes F1).
const readerPrompt = `
You are a precise question-answering system. Answer the question using ONLY the
context passages below. Reply with the shortest possible answer — a name, an
entity, a number, a date, or exactly "yes"/"no" — and nothing else. Do not write
a sentence, an explanation, or any punctuation beyond what the answer needs. If
the context does not contain the answer, give your single best guess from it.

## Question

{{ .Query }}

## Context

{{ range .Passages }}- {{ . }}
{{ end }}`

// evaluateGeneration runs the end-to-end reader evaluation: for each query that
// carries gold answers, it retrieves the top-k passages, asks the chat model for
// a short answer grounded in them, and scores it with EM/F1. It is invoked from
// evaluateCorpus when AMOXTLI_EVAL_GENERATE is set. retrieve returns ranked
// source ids; their text is looked up in the in-memory corpus (no store round
// trip). mode labels the retrieval configuration for the report.
func evaluateGeneration(
	t *testing.T,
	ctx context.Context,
	corpus *eval.Corpus,
	dataset *eval.Dataset,
	retrieve func(ctx context.Context, query string, k int) ([]string, error),
	mode string,
) {
	t.Helper()

	chat := chatClient(t)

	contentBySource := make(map[string]string, len(corpus.Documents))
	for _, d := range corpus.Documents {
		txt := d.Content
		if d.Title != "" {
			txt = d.Title + "\n" + d.Content
		}
		contentBySource[d.Source] = txt
	}

	contextK := envInt(t, "AMOXTLI_EVAL_GEN_CONTEXT_K", 5)
	maxWords := envInt(t, "AMOXTLI_EVAL_GEN_MAX_WORDS", 250)
	progressEvery := time.Duration(envInt(t, "AMOXTLI_EVAL_PROGRESS_SECONDS", 15)) * time.Second

	var scored, skipped int
	var sumEM, sumF1 float64
	start := time.Now()
	lastLog := start

	for _, q := range dataset.Queries {
		if len(q.Answers) == 0 {
			skipped++
			continue
		}

		ids, err := retrieve(ctx, q.Query, contextK)
		if err != nil {
			t.Fatalf("generation: retrieving %q: %+v", queryLabelOrID(q), errors.WithStack(err))
		}
		passages := make([]string, 0, len(ids))
		for _, id := range ids {
			if c, ok := contentBySource[id]; ok {
				passages = append(passages, truncateWords(c, maxWords))
			}
		}

		answer, err := generateAnswer(ctx, chat, q.Query, passages)
		if err != nil {
			t.Fatalf("generation: answering %q: %+v", queryLabelOrID(q), errors.WithStack(err))
		}

		sumEM += eval.AnswerExactMatch(answer, q.Answers)
		sumF1 += eval.AnswerF1(answer, q.Answers)
		scored++

		if progressEvery > 0 && time.Since(lastLog) >= progressEvery {
			t.Logf("generation progress: %d scored, elapsed %s", scored, time.Since(start).Round(time.Second))
			lastLog = time.Now()
		}
	}

	if scored == 0 {
		t.Logf("generation: no gold answers in the dataset — nothing to score "+
			"(skipped %d queries). Provide Query.Answers to enable EM/F1.", skipped)
		return
	}

	em := sumEM / float64(scored)
	f1 := sumF1 / float64(scored)
	t.Logf("\n=== GENERATION (%s, reader over top-%d) ===\n"+
		"  scored %d queries (%d skipped: no gold answer)\n"+
		"  EM: %.3f   F1: %.3f",
		mode, contextK, scored, skipped, em, f1)

	if path := os.Getenv("AMOXTLI_EVAL_GEN_SUMMARY_FILE"); path != "" {
		line := fmt.Sprintf("%s\t%s +gen\t%d\t%.4f\t%.4f\n", dataset.Name, mode, scored, em, f1)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open gen summary file: %+v", errors.WithStack(err))
		}
		defer f.Close()
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("write gen summary: %+v", errors.WithStack(err))
		}
	}
}

// generateAnswer asks the chat model for a short answer grounded in the passages.
func generateAnswer(ctx context.Context, chat llm.Client, query string, passages []string) (string, error) {
	p, err := prompt.Template(readerPrompt, struct {
		Query    string
		Passages []string
	}{Query: query, Passages: passages})
	if err != nil {
		return "", errors.WithStack(err)
	}

	// Deterministic decoding so a re-run reproduces the same answers (and, with a
	// warm cache upstream, the same score).
	seed, err := text.IntHash(p)
	if err != nil {
		return "", errors.WithStack(err)
	}

	completion, err := chat.ChatCompletion(ctx,
		llm.WithMessages(llm.NewMessage(llm.RoleUser, p)),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return strings.TrimSpace(completion.Message().Content()), nil
}

// truncateWords caps a passage to the first n whitespace-separated words, to
// bound the reader's context size. A non-positive n leaves it unchanged.
func truncateWords(s string, n int) string {
	if n <= 0 {
		return s
	}
	fields := strings.Fields(s)
	if len(fields) <= n {
		return strings.Join(fields, " ")
	}
	return strings.Join(fields[:n], " ")
}

func queryLabelOrID(q eval.Query) string {
	if q.ID != "" {
		return q.ID
	}
	return q.Query
}
