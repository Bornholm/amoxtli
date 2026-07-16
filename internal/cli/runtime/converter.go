package runtime

import (
	"context"
	"os/exec"

	"github.com/bornholm/amoxtli/convert"
	convgenai "github.com/bornholm/amoxtli/convert/genai"
	"github.com/bornholm/amoxtli/convert/libreoffice"
	"github.com/bornholm/amoxtli/convert/pandoc"
	"github.com/bornholm/amoxtli/internal/cli/config"
	extractprovider "github.com/bornholm/genai/extract/provider"
	"github.com/pkg/errors"

	// Register the extraction backends addressable by the genai converter DSN.
	_ "github.com/bornholm/genai/extract/provider/marker"
	_ "github.com/bornholm/genai/extract/provider/mistral"
)

// newFileConverter builds the file converter from the configuration, or nil
// when only native markdown indexing is available. Converters are tried in
// registration order (see convert.Routed): the GenAI converter — routed to
// its explicitly configured extensions — comes first, then the pandoc or
// LibreOffice base converter.
func newFileConverter(ctx context.Context, cfg *config.Config) (convert.Converter, error) {
	var converters []convert.Converter

	if cfg.Converter.GenAI.Enabled {
		genaiConv, err := newGenAIConverter(ctx, cfg.Converter.GenAI)
		if err != nil {
			return nil, err
		}
		converters = append(converters, genaiConv)
	}

	base, err := newBaseConverter(cfg)
	if err != nil {
		return nil, err
	}
	if base != nil {
		converters = append(converters, base)
	}

	if len(converters) == 0 {
		return nil, nil
	}

	return convert.NewRouted(converters...), nil
}

// newBaseConverter selects the document converter: LibreOffice (a pandoc
// superset adding .doc) when enabled, otherwise standalone pandoc.
func newBaseConverter(cfg *config.Config) (convert.Converter, error) {
	pandocAvailable := binaryAvailable("pandoc")
	libreOfficeAvailable := binaryAvailable("libreoffice")

	if cfg.Converter.LibreOffice.Enabled == config.ToggleTrue {
		if !libreOfficeAvailable {
			return nil, errors.New("converter.libreoffice.enabled is true but the libreoffice binary was not found in the PATH")
		}
		if !pandocAvailable {
			return nil, errors.New("converter.libreoffice requires the pandoc binary, which was not found in the PATH")
		}
	}

	// LibreOffice needs pandoc too, so only auto-enable it when both exist.
	if cfg.Converter.LibreOffice.Enabled.Resolve(libreOfficeAvailable && pandocAvailable) {
		return libreoffice.NewConverter(pandoc.NewConverter()), nil
	}

	if cfg.Converter.Pandoc.Enabled == config.ToggleTrue && !pandocAvailable {
		return nil, errors.New("converter.pandoc.enabled is true but the pandoc binary was not found in the PATH")
	}

	if cfg.Converter.Pandoc.Enabled.Resolve(pandocAvailable) {
		return pandoc.NewConverter(), nil
	}

	return nil, nil
}

// newGenAIConverter builds the OCR/LLM converter from its DSN and the set of
// extensions it should handle.
func newGenAIConverter(ctx context.Context, cfg config.GenAIConverterConfig) (convert.Converter, error) {
	client, _, err := extractprovider.Create(ctx, extractprovider.WithTextClientDSN(cfg.DSN))
	if err != nil {
		return nil, errors.Wrap(err, "could not create genai extraction client")
	}

	return convgenai.NewConverter(client, cfg.Extensions...), nil
}

func binaryAvailable(name string) bool {
	_, err := exec.LookPath(name)

	return err == nil
}
