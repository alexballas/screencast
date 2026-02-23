//go:build !windows

package processutil

import "os/exec"

func HideConsoleWindow(cmd *exec.Cmd) {
	_ = cmd
}
