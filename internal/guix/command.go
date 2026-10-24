// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
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

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Guix starts builders and inferior Guile processes. Cancel the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
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
