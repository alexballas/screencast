package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"go2tv.app/screencast/internal/processutil"
)

// LaunchOptions is what the caller adds to a prepared pipeline.
type LaunchOptions struct {
	// MuxerArgs terminate the ffmpeg command line: the output format, its
	// options and where the output goes. Everything before them is shared.
	MuxerArgs []string
	// Stdout gives ffmpeg a pipe rather than discarding its standard output,
	// and hands the read end back on the session. Callers muxing to a file or
	// a directory leave it false.
	Stdout bool
	// Cleanup runs last in Close, once ffmpeg is dead and every handle is
	// released. Whatever the muxer arguments wrote into is the caller's to
	// remove, and it can only be removed at that point: a directory taken away
	// from a live ffmpeg is still being written to.
	Cleanup func() error
}

// Session is a launched pipeline: ffmpeg running, with the pacer and the audio
// relay feeding it.
type Session struct {
	prep    *Prepared
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  *LockedBuffer
	cleanup func() error
	done    chan error

	closeOnce sync.Once
}

// Launch starts ffmpeg on the prepared pipeline. On failure it tears the whole
// thing down, cleanup included, and the Prepared must not be used again.
func (p *Prepared) Launch(opts LaunchOptions) (*Session, error) {
	args := p.args
	args.MuxerArgs = opts.MuxerArgs
	full := FFmpegArgs(args)

	stderr := &LockedBuffer{}
	stderrWriter := io.Writer(stderr)
	if p.cfg.LogOutput != nil {
		stderrWriter = io.MultiWriter(p.cfg.LogOutput, stderrWriter)
	}
	if p.cfg.DebugCommand {
		out := p.cfg.LogOutput
		if out == nil {
			out = os.Stderr
		}
		_, _ = fmt.Fprintf(out, "screencast ffmpeg: %s %s\n", p.cfg.FFmpegPath, strings.Join(full, " "))
	}

	cmd := exec.Command(p.cfg.FFmpegPath, full...)
	cmd.Stdin = p.video
	cmd.Stderr = stderrWriter
	processutil.HideConsoleWindow(cmd)

	s := &Session{
		prep:    p,
		cmd:     cmd,
		stderr:  stderr,
		cleanup: opts.Cleanup,
		done:    make(chan error, 1),
	}

	if opts.Stdout {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("screencast ffmpeg stdout: %w", err)
		}
		s.stdout = stdout
	}

	if err := cmd.Start(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("screencast ffmpeg start: %w", err)
	}

	runtime.SetFinalizer(s, func(sess *Session) {
		_ = sess.Close()
	})

	go func(c *exec.Cmd, done chan error) {
		done <- c.Wait()
		close(done)
		// Ensure resources are reclaimed even if caller forgets to Close after ffmpeg exits.
		_ = s.Close()
	}(cmd, s.done)

	return s, nil
}

// Done delivers ffmpeg's exit status once, then closes.
func (s *Session) Done() <-chan error {
	if s == nil {
		return nil
	}
	return s.done
}

// Stdout is the read end of ffmpeg's standard output, or nil unless the launch
// asked for it. The session owns ffmpeg's lifetime, not the reader.
func (s *Session) Stdout() io.ReadCloser {
	if s == nil {
		return nil
	}
	return s.stdout
}

// DroppedFrames reports how many video frames the pacer gave up on.
func (s *Session) DroppedFrames() int64 {
	if s == nil {
		return 0
	}
	return s.prep.DroppedFrames()
}

// StderrTail returns the last n bytes ffmpeg wrote to stderr.
func (s *Session) StderrTail(n int) string {
	if s == nil || s.stderr == nil {
		return ""
	}
	return s.stderr.Tail(n)
}

// Close stops everything, in an order that is load-bearing rather than
// incidental, and can be entered more than once: the caller, the goroutine
// waiting on ffmpeg and the finalizer all reach it.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}

	var out error
	s.closeOnce.Do(func() {
		runtime.SetFinalizer(s, nil)

		// ffmpeg goes first, before anything it reads or writes is closed. It
		// holds the video pipe and the audio socket, and taking those away from
		// a live process is a teardown with different rules than this one.
		if s.cmd != nil && s.cmd.Process != nil {
			err := s.cmd.Process.Kill()
			if err != nil && !errors.Is(err, os.ErrProcessDone) {
				out = errors.Join(out, err)
			}
		}

		out = errors.Join(out, s.prep.Abort())

		// Last, with ffmpeg dead and the pipeline unwound.
		if s.cleanup != nil {
			out = errors.Join(out, s.cleanup())
		}
	})

	return out
}
