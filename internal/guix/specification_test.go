// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Evaluate generated deployment definitions with real Guix, without building or
// contacting a host: GUIX_EVALUATION=1 go test ./internal/guix -run Specification
func TestDeploymentSpecificationEvaluation(t *testing.T) {
	if os.Getenv("GUIX_EVALUATION") != "1" {
		t.Skip("set GUIX_EVALUATION=1 to evaluate specifications with installed Guix")
	}
	const prelude = `(use-modules (gnu) (gnu image) (guix gexp))
(define asset (local-file "fixture.txt"))
(unless (file-exists? (local-file-absolute-file-name asset))
  (error "relative source asset was not found"))
(define fixture-os
  (operating-system
    (host-name "fixture")
    (timezone "Etc/UTC")
    (locale "en_US.utf8")
    (bootloader (bootloader-configuration
                 (bootloader grub-bootloader)
                 (targets '("/dev/sda"))))
    (file-systems (cons (file-system
                         (device "/dev/sda1")
                         (mount-point "/")
                         (type "ext4"))
                       %base-file-systems))))
`
	for _, tc := range []struct {
		name, value string
		invalid     bool
	}{
		{"os", "fixture-os", false},
		{"image", "(image (format 'disk-image) (name 'fixture-image) (operating-system fixture-os))", false},
		{"invalid", "42", true},
		{"image-without-os", "(image (format 'disk-image) (operating-system #f))", true},
	} {
		for _, fromFile := range []bool{false, true} {
			name := tc.name + "/inline"
			if fromFile {
				name = tc.name + "/loaded-file"
			}
			t.Run(name, func(t *testing.T) {
				source := Source{Configuration: prelude + tc.value, System: "x86_64-linux", Files: map[string]string{"fixture.txt": "asset"}}
				if fromFile {
					source.Files = map[string]string{"hosts/spec.scm": source.Configuration, "hosts/fixture.txt": "asset"}
					source.Configuration = `(load "hosts/spec.scm")`
				}
				called := false
				runner := runnerFunc(func(ctx context.Context, dir, _ string, _ ...string) (string, string, error) {
					called = true
					check := `(use-modules (gnu machine) (gnu system))
(define machines (load ` + SchemeString(filepath.Join(dir, "machines.scm")) + `))
(unless (string=? (operating-system-host-name
                    (machine-operating-system (car machines)))
                  "fixture")
  (error "deployment selected the wrong operating system"))
`
					file := filepath.Join(dir, "check.scm")
					if err := os.WriteFile(file, []byte(check), 0600); err != nil {
						t.Fatal(err)
					}
					return (ExecRunner{}).Run(ctx, dir, "guix", "repl", "-L", dir, file)
				})
				err := (Client{Runner: runner}).Deploy(t.Context(), source, Destination{Address: "192.0.2.1", User: "root", Port: 22, IdentityFile: "/tmp/fixture-identity", HostKey: "fixture-host-key"})
				if !called || (err != nil) != tc.invalid {
					t.Fatalf("configuration evaluation: called=%t err=%v", called, err)
				}
				if tc.invalid && !strings.Contains(err.Error(), "configuration must evaluate to an operating-system or an image containing one") {
					t.Fatalf("unexpected evaluation failure: %v", err)
				}
			})
		}
	}
}
