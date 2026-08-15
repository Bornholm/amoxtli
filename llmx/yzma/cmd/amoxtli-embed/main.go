// Command amoxtli-embed serves an OpenAI-compatible /v1/embeddings endpoint
// backed by llama.cpp through yzma.
//
// It exists so the evaluation harness — and anything else already speaking to
// an embeddings endpoint — can get the batched, quantised, GPU-offloaded
// throughput without the parent amoxtli module taking on yzma's dependencies or
// its Go 1.26 requirement. Point the harness at it and nothing else changes:
//
//	amoxtli-embed -model mxbai-embed-large-q8_0.gguf -lib /path/to/llama.cpp
//	export AMOXTLI_EVAL_EMBED_BASE_URL=http://localhost:8081/v1/
//	export AMOXTLI_EVAL_EMBED_MODEL=mxbai-embed-large
//
// Callers wanting to skip HTTP entirely can use the package directly instead:
// it implements genai's llm.Client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bornholm/amoxtli/llmx/yzma"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		modelPath = flag.String("model", "", "path to the GGUF embeddings model (required)")
		libPath   = flag.String("lib", "", "directory holding the llama.cpp shared libraries (empty: yzma's default lookup)")
		addr      = flag.String("addr", "127.0.0.1:8081", "address to listen on")
		modelName = flag.String("name", "", "model name reported in responses (defaults to the -model basename)")
		maxTokens = flag.Int("max-tokens", yzma.DefaultMaxSequenceTokens, "maximum tokens per input")
		maxSeqs   = flag.Int("batch", yzma.DefaultMaxSequences, "inputs packed into one llama.cpp batch")
		gpuLayers = flag.Int("gpu-layers", yzma.DefaultGPULayers, "transformer layers offloaded to the GPU (0: CPU only)")
		threads   = flag.Int("threads", 0, "CPU threads (0: llama.cpp default)")
		verbose   = flag.Bool("verbose", false, "let llama.cpp log to stderr")
	)
	flag.Parse()

	if *modelPath == "" {
		flag.Usage()
		return errors.New("-model is required")
	}

	name := *modelName
	if name == "" {
		name = baseName(*modelPath)
	}

	client, err := yzma.New(*modelPath,
		yzma.WithLibraryPath(*libPath),
		yzma.WithMaxSequenceTokens(*maxTokens),
		yzma.WithMaxSequences(*maxSeqs),
		yzma.WithGPULayers(*gpuLayers),
		yzma.WithThreads(*threads),
		yzma.WithVerbose(*verbose),
	)
	if err != nil {
		return err
	}
	defer client.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/embeddings", embeddingsHandler(client, name))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Embedding a full batch on CPU can take a while; a short write timeout
		// would cut the response off mid-vector.
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("amoxtli-embed listening",
		slog.String("addr", *addr),
		slog.String("model", name),
		slog.Int("dimensions", client.Dimensions()),
		slog.Int("batch", *maxSeqs),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// request is the subset of the OpenAI embeddings request this server honours.
// Input is left raw because the API accepts either a string or an array of
// them, and clients use both.
type request struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

type embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type usage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type response struct {
	Object string      `json:"object"`
	Data   []embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  usage       `json:"usage"`
}

func embeddingsHandler(client *yzma.Client, modelName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("decoding request: %v", err))
			return
		}

		inputs, err := decodeInputs(req.Input)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(inputs) == 0 {
			writeError(w, http.StatusBadRequest, "input is empty")
			return
		}

		res, err := client.Embeddings(r.Context(), inputs)
		if err != nil {
			slog.ErrorContext(r.Context(), "embeddings failed", slog.Any("error", err), slog.Int("inputs", len(inputs)))
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		vectors := res.Embeddings()
		data := make([]embedding, len(vectors))
		for i, vec := range vectors {
			data[i] = embedding{Object: "embedding", Index: i, Embedding: vec}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{
			Object: "list",
			Data:   data,
			Model:  modelName,
			Usage: usage{
				PromptTokens: res.Usage().PromptTokens(),
				TotalTokens:  res.Usage().TotalTokens(),
			},
		})
	}
}

// decodeInputs accepts both shapes the OpenAI API allows for "input": a single
// string, or an array of them. Token-ID arrays are not supported — the caller
// would have had to tokenise with this exact model to produce them.
func decodeInputs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("input is missing")
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}

	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}

	return nil, errors.New("input must be a string or an array of strings")
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "invalid_request_error"},
	})
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
