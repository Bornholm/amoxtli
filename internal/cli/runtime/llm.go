package runtime

import (
	"context"

	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/mistral"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/bornholm/genai/llm/provider/openrouter"
	"github.com/pkg/errors"
)

// newLLMClient builds a single genai client serving the configured chat
// completion and embeddings endpoints, or nil when neither is configured.
func newLLMClient(ctx context.Context, cfg *config.Config) (llm.Client, error) {
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

	return client, nil
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
