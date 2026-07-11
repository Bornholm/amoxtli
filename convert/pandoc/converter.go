package pandoc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bornholm/amoxtli/convert"
	"github.com/pkg/errors"
)

type Converter struct {
}

// Convert implements convert.Converter.
func (f *Converter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	tempDir, err := os.MkdirTemp(os.TempDir(), "amoxtli-*")
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer os.RemoveAll(tempDir)

	ext := filepath.Ext(filename)

	source := filepath.Join(tempDir, "file"+ext)
	target := filepath.Join(tempDir, "file.md")

	copy, err := os.Create(source)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if _, err := io.Copy(copy, r); err != nil {
		return nil, errors.WithStack(err)
	}

	cmd := exec.Command("pandoc", "--to", "commonmark-raw_html", "--output", target, source)

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return nil, errors.WithStack(err)
	}

	markdown, err := os.Open(target)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return markdown, nil
}

// SupportedExtensions implements convert.Converter.
func (f *Converter) SupportedExtensions() []string {
	return []string{".docx", ".rtf", ".odt", ".md", ".rst", ".epub", ".html", ".tex", ".txt"}
}

func NewConverter() *Converter {
	return &Converter{}
}

var _ convert.Converter = &Converter{}
