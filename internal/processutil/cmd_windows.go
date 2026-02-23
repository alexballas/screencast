//go:build windows

package processutil

import (
	"os/exec"
	"syscall"
)

// HideConsoleWindow prevents an attached console window from flashing when
// launching console processes from GUI apps on Windows.
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
