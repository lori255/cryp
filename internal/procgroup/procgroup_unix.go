//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package procgroup

import (
	"os/exec"
	"syscall"
)

// Configure starts cmd in its own process group so cancellation can reap
// FFmpeg helpers as well as the top-level process.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return Kill(cmd) }
}

// Kill terminates the whole process group, falling back to the direct child
// when a process group was not created.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}
