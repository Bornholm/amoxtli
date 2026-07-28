package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newImageCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "image",
		Aliases: []string{"images"},
		Short:   "Access the images referenced by indexed documents",
		Long: "Access the images stored alongside the corpus.\n\n" +
			"Indexed documents reference their images as amoxtli://images/<hash>, a URI\n" +
			"that appears in the section contents shown by \"amoxtli search --content\".\n" +
			"These commands read the store those URIs point to.",
	}

	cmd.AddCommand(
		newImageGetCommand(opts),
		newImageListCommand(opts),
	)

	return cmd
}

// imageInfo is the JSON shape of a stored image.
type imageInfo struct {
	Hash     string `json:"hash"`
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

func newImageGetCommand(opts *rootOptions) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "get <uri|hash>",
		Short: "Write a stored image to a file or to stdout",
		Long: "Write a stored image, addressed by its amoxtli://images/<hash> URI or by\n" +
			"the bare hash.\n\n" +
			"Like curl, this refuses to write binary content to a terminal: give -o, or\n" +
			"redirect stdout (amoxtli image get <hash> > schema.png), or ask for it\n" +
			"explicitly with -o -.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withImageStore(opts, cmd, func(blobs blob.Store) error {
				hash, ok := blob.ParseURI(args[0])
				if !ok {
					return errors.Errorf("malformed image reference %q (expected %s or a bare hash)", args[0], blob.URI("<hash>"))
				}

				data, info, err := blobs.Get(cmd.Context(), hash)
				if err != nil {
					if errors.Is(err, blob.ErrNotFound) {
						return errors.Errorf("no image stored for %s", blob.URI(hash))
					}

					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), toImageInfo(*info))
				}

				return writeImage(cmd, output, data)
			})
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "", "write to this file instead of stdout (\"-\" forces stdout)")

	return cmd
}

// writeImage writes the bytes where the user asked, refusing to dump binary
// content into a terminal — the curl/git cat-file convention. Piping and
// redirecting keep working, since neither leaves stdout attached to a tty.
func writeImage(cmd *cobra.Command, output string, data []byte) error {
	if output == "" || output == "-" {
		out := cmd.OutOrStdout()

		if output == "" && isTerminal(out) {
			return errors.New("refusing to write binary output to the terminal: use -o <file>, redirect stdout, or force it with -o -")
		}

		if _, err := out.Write(data); err != nil {
			return errors.WithStack(err)
		}

		return nil
	}

	if err := os.WriteFile(output, data, 0o600); err != nil {
		return errors.WithStack(err)
	}

	// The confirmation goes to stderr so that "-o -" and a redirected stdout
	// stay byte-exact.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%s)\n", output, humanize.Bytes(uint64(len(data))))

	return nil
}

func newImageListCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the stored images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withImageStore(opts, cmd, func(blobs blob.Store) error {
				images := make([]imageInfo, 0)

				var total int64

				err := blobs.List(cmd.Context(), func(info blob.Info) error {
					images = append(images, toImageInfo(info))
					total += info.Size

					return nil
				})
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), images)
				}

				out := cmd.OutOrStdout()

				if len(images) == 0 {
					_, _ = fmt.Fprintln(out, "No image stored.")

					return nil
				}

				writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

				_, _ = fmt.Fprintln(writer, "HASH\tTYPE\tSIZE")
				for _, image := range images {
					_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\n", image.Hash, image.MimeType, humanize.Bytes(uint64(image.Size)))
				}

				if err := writer.Flush(); err != nil {
					return errors.WithStack(err)
				}

				_, _ = fmt.Fprintf(out, "\n%d image(s), %s\n", len(images), humanize.Bytes(uint64(total)))

				return nil
			})
		},
	}

	return cmd
}

func toImageInfo(info blob.Info) imageInfo {
	return imageInfo{
		Hash:     string(info.Hash),
		URI:      blob.URI(info.Hash),
		MimeType: info.MimeType,
		Size:     info.Size,
	}
}

// withImageStore opens the workspace and hands over its image store, failing
// with an actionable message when none is configured.
func withImageStore(opts *rootOptions, cmd *cobra.Command, fn func(blobs blob.Store) error) error {
	ws, cfg, err := opts.loadConfig()
	if err != nil {
		return err
	}

	rt, err := runtime.Open(cmd.Context(), ws, cfg, "image")
	if err != nil {
		return err
	}
	defer rt.Close()

	blobs := rt.Codex.Blobs()
	if blobs == nil {
		return errors.New("no image store is configured for this workspace: set images.store in config.yaml (see docs/images.md)")
	}

	return fn(blobs)
}
