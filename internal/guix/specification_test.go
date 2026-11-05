// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Evaluate generated definitions with real Guix without building or contacting
// hosts: GUIX_EVALUATION=1 go test ./internal/guix -run Specification
func TestBlueprintSpecificationEvaluation(t *testing.T) {
	if os.Getenv("GUIX_EVALUATION") != "1" {
		t.Skip("set GUIX_EVALUATION=1 to evaluate specifications with installed Guix")
	}
	const prelude = `(use-modules (gnu) (gnu image) (gnu machine) (gnu machine ssh) (guix gexp))
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
(define fixture-image
  (image (format 'disk-image) (name 'fixture-image) (operating-system fixture-os)))
(define fixture-machine
  (machine
    (operating-system fixture-os)
    (environment managed-host-environment-type)
    (configuration (machine-ssh-configuration
      (host-name "ignored.example") (system "riscv64-linux")
      (user "ignored") (port 2222) (identity "/ignored") (host-key "ignored")
      (session 'ignored-session)
      (build-locally? #f) (authorize? #f) (allow-downgrades? #t)))))
`
	for _, tc := range []struct {
		name, kind, value, failure string
	}{
		{"os", "os", "fixture-os", ""},
		{"image", "image", "fixture-image", ""},
		{"machine", "machine", "fixture-machine", ""},
		{"invalid-os", "os", "42", "blueprint.os must evaluate"},
		{"image-in-os-slot", "os", "fixture-image", "blueprint.os must evaluate"},
		{"os-in-image-slot", "image", "fixture-os", "blueprint.image must evaluate"},
		{"invalid-machine", "machine", "fixture-os", "blueprint.machine must evaluate"},
		{"invalid-environment", "machine", "(machine (inherit fixture-machine) (environment #f))", "blueprint.machine must use"},
	} {
		for _, fromFile := range []bool{false, true} {
			for _, operation := range []string{"build", "deploy"} {
				if tc.kind == "image" && operation == "deploy" {
					continue
				}
				name := tc.name + "/" + operation + "/inline"
				if fromFile {
					name = tc.name + "/" + operation + "/loaded-file"
				}
				t.Run(name, func(t *testing.T) {
					source := Source{Kind: tc.kind, Configuration: prelude + tc.value, System: "aarch64-linux", Files: map[string]string{"fixture.txt": "asset"}}
					if fromFile {
						source.Files = map[string]string{"hosts/spec.scm": source.Configuration, "hosts/fixture.txt": "asset"}
						source.Configuration = `(load "hosts/spec.scm")`
						if tc.name == "os" {
							// Match the homelab blueprint: select an OS binding from a
							// bundled file whose final expression returns an image.
							source.Files["hosts/spec.scm"] = prelude + "fixture-image"
							source.Configuration = `(begin (load "hosts/spec.scm") fixture-os)`
						}
					}
					stopped := errors.New("evaluated; stop before building")
					called := false
					runner := runnerFunc(func(ctx context.Context, dir, _ string, _ ...string) (string, string, error) {
						called = true
						check := `(use-modules (gnu machine) (gnu machine ssh) (gnu system) (gnu image))
`
						if operation == "deploy" {
							check += `(define machines (load ` + SchemeString(filepath.Join(dir, "machines.scm")) + `))
(define result (machine-operating-system (car machines)))
(define config (machine-configuration (car machines)))
(unless (and (string=? (machine-ssh-configuration-host-name config) "192.0.2.1")
             (string=? (machine-ssh-configuration-user config) "root")
             (= (machine-ssh-configuration-port config) 22)
             (string=? (machine-ssh-configuration-host-key config) "fixture-host-key")
             (string=? ((@@ (gnu machine ssh) machine-ssh-configuration-system) config) "aarch64-linux")
             (string=? ((@@ (gnu machine ssh) machine-ssh-configuration-identity) config) "/tmp/fixture-identity")
             (not (machine-ssh-configuration-session config)))
  (error "host connection settings were not applied"))
`
							if tc.kind == "machine" {
								check += `(unless (and (not (machine-ssh-configuration-build-locally? config))
             (not (machine-ssh-configuration-authorize? config))
             (machine-ssh-configuration-allow-downgrades? config))
  (error "machine deployment policies were lost"))
`
							}
						} else {
							check += `(define result (load ` + SchemeString(filepath.Join(dir, "configuration.scm")) + `))
`
							if tc.kind == "image" {
								check += `(set! result (image-operating-system result))
`
							}
						}
						check += `(unless (and (operating-system? result)
             (string=? (operating-system-host-name result) "fixture"))
  (error "selected the wrong operating system"))
`
						file := filepath.Join(dir, "check.scm")
						if err := os.WriteFile(file, []byte(check), 0600); err != nil {
							t.Fatal(err)
						}
						out, stderr, err := (ExecRunner{}).Run(ctx, dir, "guix", "repl", "-q", "-L", dir, "--", file)
						if err == nil {
							err = stopped
						}
						return out, stderr, err
					})
					client := Client{Runner: runner}
					var err error
					if operation == "deploy" {
						err = client.Deploy(t.Context(), source, Destination{Address: "192.0.2.1", User: "root", Port: 22, IdentityFile: "/tmp/fixture-identity", HostKey: "fixture-host-key"})
					} else if tc.kind == "image" {
						_, _, err = client.BuildImage(t.Context(), source, "", "", filepath.Join(t.TempDir(), "image"))
					} else {
						_, err = client.BuildSystem(t.Context(), source)
					}
					if !called {
						t.Fatal("definition was not evaluated")
					}
					if tc.failure == "" && !errors.Is(err, stopped) {
						t.Fatalf("evaluation failed: %v", err)
					}
					if tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)) {
						t.Fatalf("expected %q: %v", tc.failure, err)
					}
				})
			}
		}
	}
}
