// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Runner keeps command execution injectable and stdout separate from diagnostics.
type Runner interface {
	Run(context.Context, string, string, ...string) (string, string, error)
}

type ExecRunner struct{}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64*1024 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

type subprocessLog struct {
	ctx             context.Context
	command, stream string
	pending         bytes.Buffer
}

func (w *subprocessLog) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		// Bound unfinished lines as well as the separately captured output.
		chunk := p[:min(len(p), 64*1024-w.pending.Len())]
		if end := bytes.IndexAny(chunk, "\r\n"); end >= 0 {
			w.pending.Write(chunk[:end])
			w.flush()
			p = p[end+1:]
		} else {
			w.pending.Write(chunk)
			p = p[len(chunk):]
			if w.pending.Len() == 64*1024 {
				w.flush()
			}
		}
	}
	return n, nil
}

func (w *subprocessLog) flush() {
	if w.pending.Len() > 0 {
		tflog.Info(w.ctx, w.pending.String(), map[string]any{
			"command": w.command, "stream": w.stream,
		})
		w.pending.Reset()
	}
}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	done := make(chan struct{})
	cmd.Dir = dir
	// Guix starts builders and inferior Guile processes. Cancel the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if slices.Contains(args, "secrets") {
			// Let the extension unwind mounts before escalating cancellation.
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			go func() {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				}
			}()
			return err
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 7 * time.Second
	var out, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if !slices.Contains(args, "secrets") || slices.Contains(args, "--help") {
		// Private Scheme evaluated by guix secrets may print sensitive contents.
		// Its --help preflight evaluates no secrets; retain time-machine progress.
		stdoutLog := &subprocessLog{ctx: ctx, command: name, stream: "stdout"}
		stderrLog := &subprocessLog{ctx: ctx, command: name, stream: "stderr"}
		cmd.Stdout, cmd.Stderr = io.MultiWriter(&out, stdoutLog), io.MultiWriter(&stderr, stderrLog)
		defer stdoutLog.flush()
		defer stderrLog.flush()
	}
	err := cmd.Run()
	close(done)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.String(), stderr.String(), err
}

func run(ctx context.Context, runner Runner, dir, binary string, args ...string) (string, error) {
	out, stderr, err := runner.Run(ctx, dir, binary, args...)
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", binary, err, stderr)
	}
	return out, nil
}
