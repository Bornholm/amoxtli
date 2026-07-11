package libreoffice

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/convert/pandoc"
	"github.com/pkg/errors"
)

type Converter struct {
	pandoc *pandoc.Converter
}

// Convert implements convert.Converter.
func (f *Converter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	if filepath.Ext(filename) != ".doc" {
		return f.pandoc.Convert(ctx, filename, r)
	}

	tempDir, err := os.MkdirTemp(os.TempDir(), "amoxtli-*")
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer os.RemoveAll(tempDir)

	source := filepath.Join(tempDir, "file.doc")
	target := filepath.Join(tempDir, "file.docx")

	copy, err := os.Create(source)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if _, err := io.Copy(copy, r); err != nil {
		return nil, errors.WithStack(err)
	}

	cmd := exec.Command("libreoffice", "--headless", "--convert-to", "docx", source, "--outdir", tempDir)

	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return nil, errors.WithStack(err)
	}

	docx, err := os.Open(target)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer docx.Close()

	filename = strings.TrimSuffix(filename, ".doc")

	return f.pandoc.Convert(ctx, filename+".docx", docx)
}

// SupportedExtensions implements convert.Converter.
func (f *Converter) SupportedExtensions() []string {
	return append(f.pandoc.SupportedExtensions(), ".doc")
}

func NewConverter(pandoc *pandoc.Converter) *Converter {
	return &Converter{
		pandoc: pandoc,
	}
}

var _ convert.Converter = &Converter{}
