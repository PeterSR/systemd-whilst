package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/value"
)

func normalize(v any) any { return value.Normalize(v) }

// command runs a shell command and takes its stdout, exit status or JSON
// output as the value.
type command struct {
	cfg config.Command
}

func (c *command) describe() string {
	return fmt.Sprintf("command (%s): %s", c.cfg.Output, c.cfg.Run)
}

func (c *command) watch(context.Context, func(any), func()) error { return nil }

func (c *command) read(ctx context.Context) (any, error) {
	stdout, stderr, code, err := RunShell(ctx, c.cfg.Run, c.cfg.Timeout, nil, true)
	if err != nil {
		return nil, err
	}
	switch c.cfg.Output {
	case "status":
		return code == 0, nil
	case "json":
		if code != 0 {
			return nil, exitError(code, stderr)
		}
		dec := json.NewDecoder(bytes.NewReader(stdout))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("command output is not JSON: %w", err)
		}
		return v, nil
	default:
		if code != 0 {
			return nil, exitError(code, stderr)
		}
		return strings.TrimSpace(string(stdout)), nil
	}
}

func exitError(code int, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		return fmt.Errorf("command exited with status %d", code)
	}
	return fmt.Errorf("command exited with status %d: %s", code, msg)
}

// RunShell runs a command with sh -c in its own process group, killing the
// whole group on timeout or cancellation. err is set only when the command
// could not run to completion; a non-zero exit is reported through code.
//
// With capture set, stdout and stderr are returned. A background job started
// by the command that keeps those pipes open is cut off shortly after the
// shell exits. Without capture, output goes to whilst's own stderr (the
// journal), so a command like `app &` returns as soon as the shell exits and
// the app keeps running.
func RunShell(ctx context.Context, script string, timeout time.Duration, env []string, capture bool) (stdout, stderr []byte, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = nil
	var out, errb bytes.Buffer
	if capture {
		cmd.Stdout = &limited{b: &out, n: 1 << 20}
		cmd.Stderr = &limited{b: &errb, n: 64 << 10}
		cmd.WaitDelay = 2 * time.Second
	} else {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		if errors.Is(werr, exec.ErrWaitDelay) {
			// The command exited 0 but a background job still held its
			// output; the pipes were closed. (A non-zero exit is reported
			// as an ExitError instead.)
			werr = nil
		}
		if werr != nil {
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					return out.Bytes(), errb.Bytes(), -1, fmt.Errorf("command killed by %s", ws.Signal())
				}
				return out.Bytes(), errb.Bytes(), ee.ExitCode(), nil
			}
			return out.Bytes(), errb.Bytes(), -1, werr
		}
		return out.Bytes(), errb.Bytes(), 0, nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out.Bytes(), errb.Bytes(), -1, fmt.Errorf("command timed out after %s", timeout)
		}
		return out.Bytes(), errb.Bytes(), -1, ctx.Err()
	}
}

// limited is a writer that keeps at most n bytes and discards the rest.
type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

// file reads a small file and takes its trimmed contents as a string.
type file struct {
	path string
}

func (f *file) describe() string { return "file " + f.path }

func (f *file) watch(context.Context, func(any), func()) error { return nil }

func (f *file) read(ctx context.Context) (any, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, 64<<10))
	if err != nil {
		return nil, err
	}
	return strings.TrimSpace(string(data)), nil
}
