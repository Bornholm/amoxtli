// Package yzma provides an in-process embeddings client backed by llama.cpp
// through [yzma], as an alternative to going through an Ollama or
// OpenAI-compatible HTTP endpoint.
//
// It exists for one reason: speed. On the development machine, embedding a
// corpus through Ollama runs at 0.30 documents/s; the same model quantised to
// Q8_0 on a Vulkan build of llama.cpp reaches 1.78 documents/s — a 5.9x
// speed-up that turns a 35-minute evaluation ingestion into a 6-minute one, and
// measured again end-to-end on a 3633-passage corpus: 24m28s against roughly
// 2h30. Most of that gap is not the backend but the batching: Ollama serves
// /v1/embeddings one request at a time, while this client packs as many inputs
// into a single llama.cpp batch as the physical batch size allows.
//
// Two findings worth keeping next to those numbers. On GPU, Q8_0 is the optimum
// and Q4_0 regresses — the work is compute-bound, and dequantizing int4 costs
// more than the bandwidth it saves; on CPU the order reverses, thanks to
// AVX-VNNI. And llama.cpp's OpenVINO backend does not support encoders at all,
// so it is not a route for embeddings or reranking models.
//
// yzma calls llama.cpp through purego and libffi, so no cgo and no C compiler is
// involved — `go build` stays a plain `go build`. What it does need, at run time
// only, is the llama.cpp shared libraries; nothing here loads them until a
// Client is actually constructed, so a consumer of amoxtli that never touches
// this package pays only for the module requirements (and the Go 1.26 minimum
// yzma brings with it).
//
// [yzma]: https://github.com/hybridgroup/yzma
package yzma

import (
	"sync"

	"github.com/hybridgroup/yzma/pkg/llama"
	"github.com/pkg/errors"
)

// The llama.cpp shared libraries and its backend registry are process-global:
// loading the libraries twice, or from two different paths, is not something the
// C library supports. The first client to be constructed therefore wins, and a
// later one asking for a different path is told so rather than silently getting
// the already-loaded one.
var (
	runtimeOnce sync.Once
	runtimePath string
	runtimeErr  error
)

// loadRuntime loads the llama.cpp libraries and initialises the backend exactly
// once for the process. libraryPath may be empty, in which case yzma looks in
// its default locations.
func loadRuntime(libraryPath string, verbose bool) error {
	runtimeOnce.Do(func() {
		if err := llama.Load(libraryPath); err != nil {
			runtimeErr = errors.Wrapf(err, "yzma: loading llama.cpp libraries from %q", libraryPath)
			return
		}

		// llama.cpp logs model metadata and per-batch timings to stderr by
		// default, which drowns the host application's own logs.
		if !verbose {
			llama.LogSet(llama.LogSilent())
		}

		llama.Init()
		runtimePath = libraryPath
	})

	if runtimeErr != nil {
		return runtimeErr
	}

	if libraryPath != runtimePath {
		return errors.Errorf(
			"yzma: llama.cpp is already loaded from %q, cannot also load it from %q (the library is process-global)",
			runtimePath, libraryPath,
		)
	}

	return nil
}

// Shutdown releases the process-global llama.cpp backend. It is not called by
// Client.Close, because several clients may share the backend and one closing
// must not pull it from under the others. Call it once, at process exit, after
// every Client has been closed; a program that simply exits need not call it at
// all.
func Shutdown() {
	llama.Close()
}
