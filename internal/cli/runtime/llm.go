package runtime

import (
	"context"

	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"
	"github.com/pkg/errors"
)

// newLLMClient builds a single genai client serving the configured chat
// completion and embeddings endpoints, or nil when neither is configured.
// When the LLM cache is enabled (the default as soon as a client is
// configured) it is wrapped with llmx.CachingClient: re-indexing unchanged
// content and repeated queries reuse on-disk vectors — and seeded HyDE
// completions — instead of calling the endpoints again.
func newLLMClient(ctx context.Context, ws *workspace.Workspace, cfg *config.Config) (llm.Client, error) {
	var funcs []provider.OptionFunc

	if cfg.HasChat() {
		fn, err := chatOption(cfg.LLM.Chat)
		if err != nil {
			return nil, err
		}
		funcs = append(funcs, fn)
	}

	if cfg.HasEmbeddings() {
		fn, err := embeddingsOption(cfg.LLM.Embeddings)
		if err != nil {
			return nil, err
		}
		funcs = append(funcs, fn)
	}

	if len(funcs) == 0 {
		return nil, nil
	}

	client, err := provider.Create(ctx, funcs...)
	if err != nil {
		return nil, errors.Wrap(err, "could not create llm client")
	}

	if cfg.LLMCacheEnabled() {
		// The namespaces must identify the embedding space / chat model (see
		// llmx.CachingClient): keying on the model names guarantees a model
		// switch never serves results from the wrong space.
		namespace := ""
		if cfg.HasEmbeddings() {
			namespace = cfg.LLM.Embeddings.Model
		}

		var cacheOpts []llmx.CachingOption
		if cfg.HasChat() {
			cacheOpts = append(cacheOpts, llmx.WithChatCache(cfg.LLM.Chat.Model))
		}

		cached, err := llmx.NewCachingClient(client, ws.Resolve(cfg.LLMCachePath()), namespace, cacheOpts...)
		if err != nil {
			return nil, errors.Wrap(err, "could not create llm cache")
		}
		return cached, nil
	}

	return client, nil
}

// newStageLLMClients builds one dedicated chat client per configured
// llm.stages entry (hyde, judge, ...), each overriding the default llm.chat
// client for that stage. When the LLM cache is enabled each stage client gets
// the chat cache too, keyed by its own model.
func newStageLLMClients(ctx context.Context, ws *workspace.Workspace, cfg *config.Config) (map[string]llm.Client, error) {
	if len(cfg.LLM.Stages) == 0 {
		return nil, nil
	}

	clients := make(map[string]llm.Client, len(cfg.LLM.Stages))
	for name, stageCfg := range cfg.LLM.Stages {
		if stageCfg == nil {
			continue
		}

		fn, err := chatOption(stageCfg)
		if err != nil {
			return nil, errors.Wrapf(err, "llm.stages.%s", name)
		}

		client, err := provider.Create(ctx, fn)
		if err != nil {
			return nil, errors.Wrapf(err, "could not create llm client for stage %q", name)
		}

		if cfg.LLMCacheEnabled() {
			cached, err := llmx.NewCachingClient(client, ws.Resolve(cfg.LLMCachePath()), "", llmx.WithChatCache(stageCfg.Model))
			if err != nil {
				return nil, errors.Wrapf(err, "could not create llm cache for stage %q", name)
			}
			client = cached
		}

		clients[name] = client
	}

	return clients, nil
}

// chatOption builds the provider-specific chat completion option. The
// openai/openrouter/mistral providers all share provider.CommonOptions.
func chatOption(cfg *config.ClientConfig) (provider.OptionFunc, error) {
	common := commonOptions(cfg)

	switch provider.Name(cfg.Provider) {
	case openai.Name:
		return provider.WithChatCompletion(openai.Name, openai.Options{CommonOptions: common}), nil
	case openrouter.Name:
		return provider.WithChatCompletion(openrouter.Name, openrouter.Options{CommonOptions: common}), nil
	case mistral.Name:
		return provider.WithChatCompletion(mistral.Name, mistral.Options{CommonOptions: common}), nil
	default:
		return nil, unsupportedProvider(cfg.Provider)
	}
}

// embeddingsOption builds the provider-specific embeddings option.
func embeddingsOption(cfg *config.ClientConfig) (provider.OptionFunc, error) {
	common := commonOptions(cfg)

	switch provider.Name(cfg.Provider) {
	case openai.Name:
		return provider.WithEmbeddings(openai.Name, openai.Options{CommonOptions: common}), nil
	case openrouter.Name:
		return provider.WithEmbeddings(openrouter.Name, openrouter.Options{CommonOptions: common}), nil
	case mistral.Name:
		return provider.WithEmbeddings(mistral.Name, mistral.Options{CommonOptions: common}), nil
	default:
		return nil, unsupportedProvider(cfg.Provider)
	}
}

func commonOptions(cfg *config.ClientConfig) provider.CommonOptions {
	return provider.CommonOptions{
		BaseURL: cfg.BaseURL,
		Model:   cfg.Model,
		APIKey:  cfg.APIKey,
	}
}

func unsupportedProvider(name string) error {
	return errors.Errorf("unsupported llm provider %q (supported: %v)", name, config.SupportedProviders)
}
