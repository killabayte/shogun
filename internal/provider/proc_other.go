//go:build !unix

package provider

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
