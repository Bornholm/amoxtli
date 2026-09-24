//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// isolate starts the program in its own process group, and kills the group
// on cancellation.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
