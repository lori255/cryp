//go:build windows || plan9 || js || wasip1

package procgroup

import "os/exec"

func Configure(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return Kill(cmd) }
}

func Kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
