//go:build unix

package algorithm

import (
	"os/exec"
	"syscall"
)

func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		// Kill the dedicated group so inherited pipes cannot keep Wait blocked.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}
