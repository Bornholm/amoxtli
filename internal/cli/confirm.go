package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// confirm asks a yes/no question on stdin, defaulting to no. It reads from the
// command's configured input so tests can drive it.
func confirm(cmd *cobra.Command, prompt string) (bool, error) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)

	reader := bufio.NewReader(cmd.InOrStdin())

	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, errors.WithStack(err)
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
