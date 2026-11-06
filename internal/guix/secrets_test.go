// SPDX-License-Identifier: MPL-2.0
package guix

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestProvisioningWorkspaceIsolation(t *testing.T) {
	for _, action := range []string{"image", "deploy"} {
		t.Run(action, func(t *testing.T) {
			const secret = "PRIVATE-PAYLOAD-MARKER"
			const key = "PRIVATE-MANAGEMENT-IDENTITY"
			var public, private string
			stopped := errors.New("stop")
			c := Client{Runner: runnerFunc(func(_ context.Context, dir, binary string, args ...string) (string, string, error) {
				public = dir
				if binary != "guix" || !slices.Contains(args, "time-machine") || !slices.Contains(args, "secrets") || !slices.Contains(args, action) {
					t.Fatalf("not a pinned extension invocation: %s %v", binary, args)
				}
				for _, arg := range args {
					if strings.Contains(arg, secret) || strings.Contains(arg, key) || strings.Contains(arg, "GUIX_EXTENSIONS_PATH") || strings.HasPrefix(arg, "--host=") {
						t.Fatal("private CLI data or extension extraction")
					}
					if strings.HasPrefix(arg, "--secrets=") {
						path := strings.TrimPrefix(arg, "--secrets=")
						private = filepath.Dir(path)
						body, err := os.ReadFile(path)
						if err != nil || string(body) != secret {
							t.Fatal("Scheme source lost")
						}
						info, _ := os.Stat(path)
						if info.Mode().Perm() != 0600 {
							t.Fatal("private input permissions")
						}
					}
				}
				if public == private || private == "" || strings.HasPrefix(private, public+"/") {
					t.Fatal("shared public/private workspace")
				}
				for _, directory := range []string{public, private} {
					info, err := os.Stat(directory)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatal("workspace is not private")
					}
				}
				if action == "deploy" {
					body, err := os.ReadFile(filepath.Join(private, "machines.scm"))
					if err != nil || !strings.Contains(string(body), "machine-ssh-configuration") || !strings.Contains(string(body), "192.0.2.1") {
						t.Fatal("effective machine connection missing")
					}
					body, err = os.ReadFile(filepath.Join(private, "identity"))
					if err != nil || string(body) != key {
						t.Fatal("management identity missing")
					}
				} else if !slices.Contains(args, "--image-type=custom") || !slices.Contains(args, "--artifact=/base") {
					t.Fatal("image declaration/options missing")
				}
				if !slices.Contains(args, "--system=x86_64-linux") {
					t.Fatal("declaration evaluation architecture differs from the build")
				}
				if err := filepath.WalkDir(public, func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !d.IsDir() {
						body, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						if strings.Contains(string(body), secret) || strings.Contains(string(body), key) {
							t.Fatal("private data in public source")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return "", "", stopped
			})}
			source := Source{Kind: "os", Configuration: "system", Channels: "pinned", System: "x86_64-linux"}
			secrets := SecretConfiguration{Expression: secret}
			var err error
			if action == "image" {
				_, err = c.ProvisionImage(t.Context(), source, secrets, "custom", "", "/base", "/output")
			} else {
				err = c.ProvisionHost(t.Context(), source, secrets, Destination{Address: "192.0.2.1", User: "root", Port: 22, HostKey: "public", PrivateKey: key})
			}
			if !errors.Is(err, stopped) {
				t.Fatalf("error lost: %v", err)
			}
			for _, directory := range []string{public, private} {
				if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("workspace retained after failure")
				}
			}
		})
	}
}

func TestMissingSecretsExtensionHint(t *testing.T) {
	c := Client{Runner: runnerFunc(func(_ context.Context, _ string, binary string, args ...string) (string, string, error) {
		if binary != "guix" || !slices.Contains(args, "--help") || !slices.Contains(args, "secrets") {
			t.Fatal("not checking installed extension")
		}
		return "", "unknown command", errors.New("unavailable")
	})}
	err := c.CheckSecrets(t.Context(), Source{Configuration: "system", Channels: "pinned"})
	if err == nil || !strings.Contains(err.Error(), "blueprint.channels") || !strings.Contains(err.Error(), "guix pull") {
		t.Fatalf("missing installation hint: %v", err)
	}
}

func TestPrivateSchemeCLIIsInterpretedAndErrorsAreRedacted(t *testing.T) {
	if os.Getenv("GUIX_EVALUATION") != "1" {
		t.Skip("set GUIX_EVALUATION=1 for installed Guix command evaluation")
	}
	extension, err := filepath.Abs("../../channel/modules")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GUIX_EXTENSIONS_PATH", extension)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	private := filepath.Join(t.TempDir(), "private-declaration.scm")
	const marker = "PRIVATE-DECLARATION-ERROR-MARKER"
	if err := os.WriteFile(private, []byte(`(error "`+marker+`")`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, stderr, err := (ExecRunner{}).Run(ctx, "", "guix", "secrets", "deploy", "--secrets="+private)
	if err == nil || strings.Contains(out+stderr, marker) || !strings.Contains(stderr, "provisioning failed") {
		t.Fatalf("private evaluation error was not redacted: %v %s", err, stderr)
	}
	if err := filepath.WalkDir(cache, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, filepath.Base(private)+".go") {
			t.Fatal("private Scheme was compiled into the cache")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(private, []byte(`(use-modules (guix secrets)) (secrets)`), 0600); err != nil {
		t.Fatal(err)
	}
	machines := filepath.Join(t.TempDir(), "machines.scm")
	if err := os.WriteFile(machines, []byte(`'()`), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = (ExecRunner{}).Run(ctx, "", "guix", "secrets", "deploy", "--system=x86_64-linux", "--secrets="+private, machines)
	if err != nil {
		t.Fatalf("Scheme command declarations were not loaded: %v %s", err, stderr)
	}
}

func TestProvisioningCancellationAllowsCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	out, _, err := (ExecRunner{}).Run(ctx, "", "sh", "-c",
		`trap 'printf cleanup-completed; exit 0' TERM; while :; do sleep 0.05; done`, "secrets")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out, "cleanup-completed") {
		t.Fatalf("cleanup interrupted: %q %v", out, err)
	}
}
