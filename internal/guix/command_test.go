// SPDX-License-Identifier: MPL-2.0
package guix

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

type subprocessTestLogs struct {
	sync.Mutex
	bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *subprocessTestLogs) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	if bytes.Contains(p, []byte("building")) {
		w.once.Do(func() { close(w.ready) })
	}
	return w.Buffer.Write(p)
}

func TestSubprocessStreamsInfoBeforeExit(t *testing.T) {
	logs := &subprocessTestLogs{ready: make(chan struct{})}
	ctx, cancel := context.WithTimeout(tflogtest.RootLogger(t.Context(), logs), 10*time.Second)
	defer cancel()
	release := filepath.Join(t.TempDir(), "release")
	type result struct {
		stdout, stderr string
		err            error
	}
	done := make(chan result, 1)
	go func() {
		stdout, stderr, err := (ExecRunner{}).Run(ctx, "", "sh", "-c",
			`printf 'building\rnext\n'; while [ ! -e "$1" ]; do sleep 0.01; done; printf result; printf 'warning\nlast' >&2; exit 7`, "private-argument", release)
		done <- result{stdout, stderr, err}
	}()
	select {
	case <-logs.ready:
	case <-ctx.Done():
		t.Fatal("subprocess output was not logged while the process was running")
	}
	select {
	case <-done:
		t.Fatal("subprocess exited before its output was streamed")
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err == nil || got.stdout != "building\rnext\nresult" || got.stderr != "warning\nlast" {
			t.Fatalf("captured output or exit status changed: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("subprocess did not finish")
	}
	entries, err := tflogtest.MultilineJSONDecode(&logs.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"building": "stdout", "next": "stdout", "result": "stdout", "warning": "stderr", "last": "stderr"}
	for _, entry := range entries {
		line, _ := entry["@message"].(string)
		if entry["@level"] != "info" || entry["command"] != "sh" || entry["stream"] != want[line] {
			t.Fatalf("unexpected subprocess log: %v", entry)
		}
		delete(want, line)
	}
	if len(want) != 0 || strings.Contains(logs.String(), "private-argument") {
		t.Fatalf("missing output or logged command arguments: %v", want)
	}
}

func TestSubprocessLogsBeyondCaptureLimit(t *testing.T) {
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	var capture diagnosticBuffer
	stream := &subprocessLog{ctx: ctx, command: "guix", stream: "stderr"}
	body := strings.Repeat("x", 128*1024+1)
	if _, err := io.MultiWriter(&capture, stream).Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	stream.flush()
	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, entry := range entries {
		line := entry["@message"].(string)
		if len(line) > 64*1024 || entry["@level"] != "info" {
			t.Fatal("unbounded or non-INFO subprocess log")
		}
		output.WriteString(line)
	}
	if capture.Len() != 64*1024 || output.String() != body {
		t.Fatal("capture limit truncated streamed output")
	}
}

func TestDiagnosticBufferKeepsFinalLines(t *testing.T) {
	for _, trailingNewline := range []bool{false, true} {
		var body, want strings.Builder
		body.WriteString(strings.Repeat("early output\n", 10000))
		for i := 0; i < 25; i++ {
			line := fmt.Sprintf("failure line %d", i)
			if i < 24 || trailingNewline {
				line += "\n"
			}
			body.WriteString(line)
			if i >= 5 {
				want.WriteString(line)
			}
		}
		for _, chunkSize := range []int{1, 37, body.Len()} {
			var capture diagnosticBuffer
			data := []byte(body.String())
			for len(data) > 0 {
				chunk := data[:min(chunkSize, len(data))]
				n, err := capture.Write(chunk)
				if err != nil || n != len(chunk) {
					t.Fatalf("write: %d, %v", n, err)
				}
				data = data[len(chunk):]
			}
			if capture.String() != want.String() {
				t.Fatalf("chunk size %d, trailing newline %v: got %q, want %q", chunkSize, trailingNewline, capture.String(), want.String())
			}
		}
	}
}

func TestSubprocessFailureDiagnosticKeepsTail(t *testing.T) {
	_, err := run(t.Context(), ExecRunner{}, "", "sh", "-c",
		`i=0; while [ "$i" -lt 10000 ]; do printf 'early output\n' >&2; i=$((i+1)); done; i=0; while [ "$i" -lt 20 ]; do printf 'final %s\n' "$i" >&2; i=$((i+1)); done; exit 7`)
	var want strings.Builder
	want.WriteString("sh: exit status 7: ")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&want, "final %d\n", i)
	}
	if err == nil || err.Error() != want.String() {
		t.Fatalf("unexpected diagnostic: %v", err)
	}
}

func TestSubprocessSecretsOutputIsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	out, stderr, err := (ExecRunner{}).Run(ctx, "", "sh", "-c", `printf private; printf private >&2`, "secrets")
	if err != nil || out != "private" || stderr != "private" || logs.Len() != 0 {
		t.Fatalf("secret output was lost or logged: %v", err)
	}
}

func TestSubprocessSecretsPreflightOutputIsLogged(t *testing.T) {
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	out, stderr, err := (ExecRunner{}).Run(ctx, "", "sh", "-c", `printf 'preparing pinned Guix\n' >&2`, "secrets", "--help")
	if err != nil || out != "" || stderr != "preparing pinned Guix\n" || !strings.Contains(logs.String(), "preparing pinned Guix") {
		t.Fatalf("preflight progress was lost: %v", err)
	}
}
