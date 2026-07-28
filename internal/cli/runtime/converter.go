package runtime

import (
	"context"
	"os/exec"

	"github.com/bornholm/amoxtli/blob"
	blobfs "github.com/bornholm/amoxtli/blob/fs"
	blobgorm "github.com/bornholm/amoxtli/blob/gorm"
	"github.com/bornholm/amoxtli/convert"
	convgenai "github.com/bornholm/amoxtli/convert/genai"
	"github.com/bornholm/amoxtli/convert/libreoffice"
	"github.com/bornholm/amoxtli/convert/pandoc"
	convvision "github.com/bornholm/amoxtli/convert/vision"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/amoxtli/markdown/imagetext"
	"github.com/bornholm/amoxtli/vision"
	extractprovider "github.com/bornholm/genai/extract/provider"
	"github.com/bornholm/genai/llm/provider"
	"github.com/pkg/errors"

	// Register the extraction backends addressable by the genai converter DSN.
	_ "github.com/bornholm/genai/extract/provider/marker"
	_ "github.com/bornholm/genai/extract/provider/mistral"
)

// newFileConverter builds the file converter from the configuration, or nil
// when only native markdown indexing is available. Converters are tried in
// registration order (see convert.Routed): the vision converter comes first —
// an image extension explicitly routed to a vision model must win over the
// same extension listed under converter.genai — then the GenAI converter
// routed to its explicitly configured extensions, then the pandoc or
// LibreOffice base converter.
func newFileConverter(ctx context.Context, cfg *config.Config, describer vision.Describer, blobs blob.Store) (convert.Converter, error) {
	var converters []convert.Converter

	if describer != nil {
		var visionOpts []convvision.Option
		if blobs != nil {
			visionOpts = append(visionOpts, convvision.WithBlobStore(blobs))
		}

		converters = append(converters, convvision.NewConverterWithOptions(describer, cfg.VisionExtensions(), visionOpts))
	}

	if cfg.Converter.GenAI.Enabled {
		genaiConv, err := newGenAIConverter(ctx, cfg.Converter.GenAI)
		if err != nil {
			return nil, err
		}
		converters = append(converters, genaiConv)
	}

	base, err := newBaseConverter(cfg, blobs)
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
// superset adding .doc) when enabled, otherwise standalone pandoc. Media
// extraction is turned on only when the embedded image enrichment is
// configured to consume it — inlining images as base64 otherwise only inflates
// the indexed source.
func newBaseConverter(cfg *config.Config, blobs blob.Store) (convert.Converter, error) {
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

	var pandocOpts []pandoc.OptionFunc
	if cfg.EmbeddedVisionEnabled() {
		pandocOpts = append(pandocOpts, pandoc.WithInlineMedia(cfg.Converter.Vision.MaxImageSize))

		// With a blob store the extracted media are stored once and referenced
		// by URI, instead of travelling as base64 inside the markdown.
		if blobs != nil {
			pandocOpts = append(pandocOpts, pandoc.WithBlobStore(blobs))
		}
	}

	// LibreOffice needs pandoc too, so only auto-enable it when both exist.
	if cfg.Converter.LibreOffice.Enabled.Resolve(libreOfficeAvailable && pandocAvailable) {
		return libreoffice.NewConverter(pandoc.NewConverter(pandocOpts...)), nil
	}

	if cfg.Converter.Pandoc.Enabled == config.ToggleTrue && !pandocAvailable {
		return nil, errors.New("converter.pandoc.enabled is true but the pandoc binary was not found in the PATH")
	}

	if cfg.Converter.Pandoc.Enabled.Resolve(pandocAvailable) {
		return pandoc.NewConverter(pandocOpts...), nil
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

// newVisionDescriber builds the image describer shared by the vision
// converter (standalone image files) and the enrichment of the images embedded
// in documents: a chat client pointed at a vision model, decorated with
// retries, wrapped in a description cache keyed by image content (the LLM chat
// cache cannot cache attachments, see vision.CachingDescriber). Sharing it
// means an image met twice — as a file and inside a document — is described
// once.
func newVisionDescriber(ctx context.Context, ws *workspace.Workspace, cfg *config.Config) (vision.Describer, error) {
	chatCfg := cfg.VisionChat()
	if chatCfg == nil {
		return nil, errors.New("converter.vision requires a chat client (converter.vision.chat or llm.chat)")
	}

	fn, err := chatOption(chatCfg)
	if err != nil {
		return nil, errors.Wrap(err, "converter.vision.chat")
	}

	client, err := provider.Create(ctx, fn)
	if err != nil {
		return nil, errors.Wrap(err, "could not create vision llm client")
	}

	var describer vision.Describer = vision.NewLLMDescriber(
		llmx.NewRetryClient(client),
		vision.WithPrompt(cfg.Converter.Vision.Prompt),
		vision.WithMaxImageBytes(cfg.Converter.Vision.MaxImageSize),
	)

	// The description cache follows the LLM cache toggle, but not its
	// activation rule: the vision converter may have its own chat client, with
	// neither llm.chat nor llm.embeddings configured.
	if cfg.LLM.Cache.Enabled.Resolve(true) {
		cached, err := vision.NewCachingDescriber(
			describer,
			ws.Resolve(cfg.LLMCachePath()),
			vision.Namespace(chatCfg.Model, cfg.Converter.Vision.Prompt),
		)
		if err != nil {
			return nil, errors.Wrap(err, "could not create vision description cache")
		}

		describer = cached
	}

	return describer, nil
}

// newBlobStore builds the store holding the images referenced by the indexed
// documents, or nil when image storage is disabled. The backend follows
// images.store: the database one shares the connection of the document store —
// one server to back up — while the filesystem one keeps the bytes out of a
// SQLite file that bleve and sqlite-vec already sit next to.
func newBlobStore(ws *workspace.Workspace, cfg *config.Config, store *gormStore.Store) (blob.Store, error) {
	if !cfg.ImagesEnabled() {
		return nil, nil
	}

	switch driver := cfg.ImageStoreDriver(); driver {
	case config.ImageStoreDatabase:
		return blobgorm.NewStore(store.DB(), blobgorm.WithMaxBytes(cfg.Images.MaxSize)), nil

	case config.ImageStoreFS:
		blobs, err := blobfs.NewStore(ws.Resolve(cfg.ImagesPath()), blobfs.WithMaxBytes(cfg.Images.MaxSize))
		if err != nil {
			return nil, errors.Wrap(err, "could not open image store")
		}

		return blobs, nil

	default:
		return nil, errors.Errorf("unknown image store backend %q", driver)
	}
}

// imageEnrichmentOptions maps converter.vision.embedded onto the library
// options describing the images embedded in documents.
func imageEnrichmentOptions(cfg *config.Config, describer vision.Describer) []imagetext.OptionFunc {
	embedded := cfg.Converter.Vision.Embedded

	return []imagetext.OptionFunc{
		imagetext.WithDescriber(describer),
		imagetext.WithMinDimension(embedded.MinDimensions),
		imagetext.WithMaxImagesPerDocument(embedded.MaxImagesPerDocument),
		imagetext.WithConcurrency(embedded.Concurrency),
		imagetext.WithMaxImageBytes(cfg.Converter.Vision.MaxImageSize),
	}
}

func binaryAvailable(name string) bool {
	_, err := exec.LookPath(name)

	return err == nil
}
