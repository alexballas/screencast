//go:build linux

package processutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const prSetChildSubreaper = 36

// Reap the orphaned children ourselves, not the machine's init.
func becomeSubreaper(t *testing.T) {
	t.Helper()
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Fatal(errno)
	}
	t.Cleanup(func() { _, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 0, 0) })
}

func TestCancellationKillsWrapperChildren(t *testing.T) {
	becomeSubreaper(t)
	path := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 300 & echo $! > "$1"; wait`, "wrapper", path)
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Kill(cmd); _ = cmd.Wait() }()
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for child == 0 {
		data, _ := os.ReadFile(path)
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			t.Fatal("wrapper did not start its child")
		}
		time.Sleep(time.Millisecond)
	}
	defer cleanupChild(child)
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("canceled wrapper succeeded")
	}
	awaitKilledChild(t, child)
}

func TestFFmpegDiesWithParent(t *testing.T) {
	if os.Getenv("SCREENCAST_PROCESS_DEATH_HELPER") == "1" {
		cmd := exec.Command("sleep", "300")
		Configure(cmd)
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(cmd.Process.Pid)
		os.Exit(0)
	}
	becomeSubreaper(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFFmpegDiesWithParent$")
	cmd.Env = append(os.Environ(), "SCREENCAST_PROCESS_DEATH_HELPER=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("child PID: %q: %v", output, err)
	}
	defer cleanupChild(child)
	awaitKilledChild(t, child)
}

func cleanupChild(pid int) {
	if got, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); err == nil && got == 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = syscall.Wait4(pid, nil, 0, nil)
	}
}

func awaitKilledChild(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got == pid {
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child exit = %v, want SIGKILL", status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ffmpeg child survived cancellation/parent exit")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKillReachesFFmpegAfterWrapperExits(t *testing.T) {
	becomeSubreaper(t)
	path := filepath.Join(t.TempDir(), "child")
	// The wrapper exits at once; its FFmpeg keeps the stdout pipe Wait joins.
	cmd := exec.Command("sh", "-c", `sleep 300 & echo $! > "$1"`, "wrapper", path)
	cmd.Stdout = io.Discard
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	defer func() { _ = Kill(cmd); <-waited }()
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for child == 0 || !errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		data, _ := os.ReadFile(path)
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if time.Now().After(deadline) {
			t.Fatal("wrapper did not exit after starting its child")
		}
		time.Sleep(time.Millisecond)
	}
	defer cleanupChild(child)
	_ = Kill(cmd)
	awaitKilledChild(t, child)
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait still blocked on the killed FFmpeg's pipe")
	}
}
