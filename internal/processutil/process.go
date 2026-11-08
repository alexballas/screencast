// Package processutil owns how ffmpeg children are started and stopped.
package processutil

import "os/exec"

// Configure must run before Start. It hides the console on Windows. On Unix
// the command gets its own process group, killed whole on cancel, so a wrapper
// script cannot leave a real ffmpeg behind; on Linux it also dies with its
// parent, even after a crash.
func Configure(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	configure(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return Kill(cmd) }
	}
}

// Start starts a long-lived command. On Windows it also binds the process
// tree to a Job Object, so Kill reaches a wrapper's ffmpeg after the wrapper
// exits and the tree dies with its parent. Call Release after Wait.
func Start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	contain(cmd)
	return nil
}
