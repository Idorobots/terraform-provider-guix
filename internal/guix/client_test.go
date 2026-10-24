// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	t          *testing.T
	calls      int
	definition string
}

func (r *recordingRunner) Run(_ context.Context, dir, binary string, args ...string) (string, string, error) {
	r.calls++
	if binary != "guix" || args[0] != "time-machine" || args[1] != "-C" {
		r.t.Fatalf("not a pinned invocation: %s %v", binary, args)
	}
	if contents, err := os.ReadFile(filepath.Join(dir, "modules", "example.scm")); err != nil || string(contents) != "module" {
		r.t.Fatalf("missing source bundle: %q %v", contents, err)
	}
	if args[len(args)-2] == "deploy" {
		contents, err := os.ReadFile(filepath.Join(dir, "machines.scm"))
		if err != nil {
			r.t.Fatal(err)
		}
		r.definition = string(contents)
		info, err := os.Stat(filepath.Join(dir, "identity"))
		if err != nil || info.Mode().Perm() != 0600 {
			r.t.Fatalf("private identity permissions: %v %v", info, err)
		}
	}
	return "/gnu/store/00000000000000000000000000000000-system\n", "diagnostics stay off stdout", nil
}

func TestPinnedDeploymentAndWorkspaceCleanup(t *testing.T) {
	runner := &recordingRunner{t: t}
	client := Client{Runner: runner}
	source := Source{Configuration: "(operating-system)", Channels: "pinned", System: "x86_64-linux", Files: map[string]string{"modules/example.scm": "module"}}
	_, err := client.BuildSystem(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Deploy(t.Context(), source, Destination{Address: "192.0.2.1", User: "root", Port: 22, PrivateKey: "private", HostKey: "ssh-ed25519 public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"managed-host-environment-type", `(host-name "192.0.2.1")`, `(host-key "ssh-ed25519 public")`, `(port 22)`} {
		if !strings.Contains(runner.definition, expected) {
			t.Fatalf("missing %s in %s", expected, runner.definition)
		}
	}
	// The definition embeds its isolated workspace; that directory must be gone.
	for _, line := range strings.Split(runner.definition, "\n") {
		if strings.Contains(line, "(identity ") {
			path := strings.Split(line, `"`)[1]
			if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary identity retained: %v", err)
			}
		}
	}
}

func TestSourceIsolationAndDigest(t *testing.T) {
	for _, path := range []string{"../escape", "/absolute", "a/../../escape", "a/../b", "configuration.scm", "channels.scm/nested", "machines.scm", "identity"} {
		t.Run(path, func(t *testing.T) {
			_, err := workspace(Source{Files: map[string]string{path: "bad"}})
			if err == nil {
				t.Fatal("accepted unsafe source path")
			}
		})
	}
	a := Source{Files: map[string]string{"b": "2", "a": "1"}}
	b := Source{Files: map[string]string{"a": "1", "b": "2"}}
	if a.Digest() != b.Digest() {
		t.Fatal("digest depends on map iteration")
	}
	b.Files["a"] = "changed"
	if a.Digest() == b.Digest() {
		t.Fatal("supporting file change ignored")
	}
	if SchemeString("\"\\\n") != `"\"\\\n"` {
		t.Fatal("invalid Scheme quoting")
	}
}

func TestCommandCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := (ExecRunner{}).Run(ctx, "", "sh", "-c", "sleep 30 & wait")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("process group did not cancel: %v", err)
	}
}
