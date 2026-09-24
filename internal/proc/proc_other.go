//go:build !unix

package proc

import "os/exec"

// isolate keeps the default cancellation, which kills the process only.
func isolate(cmd *exec.Cmd) {}
