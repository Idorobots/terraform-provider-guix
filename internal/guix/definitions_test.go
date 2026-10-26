// SPDX-License-Identifier: MPL-2.0

package guix

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testSystemDefinition = `(define-module (homelab platform)
  #:use-module (guix platform)
  #:export (custom-platform))
;; Populate the memoized discovery collection before extending it.
(platforms)
(define custom-platform
  (platform
    (system "homelab-linux")
    (target "aarch64-unknown-linux-gnu")
    (linux-architecture "arm64")
    (glibc-dynamic-linker "/lib/ld-linux-aarch64.so.1")))`

const testImageDefinition = `(define-module (homelab image)
  #:use-module (gnu image)
  #:use-module (gnu system image)
  #:use-module (guix platform)
  #:use-module (guix gexp)
  #:use-module (homelab support)
  #:export (first-type selected-type asset))
;; Neither the module name nor the binding name matches the image type name.
(force %image-types)
(define asset (local-file "asset.txt"))
(define first-type
  (image-type (inherit efi-raw-image-type) (name other-image-name)))
(define private-type
  (image-type (inherit efi-raw-image-type) (name 'private-raw)))
(define selected-type
  (image-type
    (name 'homelab-raw)
    (constructor (lambda (os) (error "CUSTOM-IMAGE-FOUND")))))`

const testOperatingSystem = `(use-modules (gnu) (gnu bootloader grub))
(operating-system
  (host-name "test")
  (timezone "Etc/UTC")
  (locale "en_US.utf8")
  (bootloader (bootloader-configuration
                (bootloader grub-bootloader)
                (targets '("/dev/vda"))))
  (file-systems %base-file-systems))`

func TestDefinitionInvocation(t *testing.T) {
	source := Source{System: "homelab-linux", ImageDefinition: testImageDefinition, SystemDefinition: testSystemDefinition}
	stopped := errors.New("stop before executing Guix")
	for _, operation := range []string{"image", "build", "deploy"} {
		t.Run(operation, func(t *testing.T) {
			var workspacePath string
			client := Client{Runner: runnerFunc(func(_ context.Context, dir, binary string, args ...string) (string, string, error) {
				workspacePath = dir
				command := "system"
				if operation == "deploy" {
					command = "deploy"
				}
				want := []string{"time-machine", "-C", filepath.Join(dir, "channels.scm"), "--", "repl", "-L", dir, "--", filepath.Join(dir, definitionLauncherFile), filepath.Join(dir, systemDefinitionFile), filepath.Join(dir, imageDefinitionFile), command}
				if binary != "guix" || len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
					t.Fatalf("invalid pinned definition invocation: %s %v", binary, args)
				}
				for name, want := range map[string]string{imageDefinitionFile: source.ImageDefinition, systemDefinitionFile: source.SystemDefinition, definitionLauncherFile: definitionLauncher} {
					contents, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(contents) != want {
						t.Fatalf("definition %s was not materialized: %v", name, err)
					}
				}
				return "", "", stopped
			})}
			var err error
			switch operation {
			case "image":
				_, _, err = client.BuildImage(t.Context(), source, "homelab-raw", filepath.Join(t.TempDir(), "image"))
			case "build":
				_, err = client.BuildSystem(t.Context(), source)
			case "deploy":
				err = client.Deploy(t.Context(), source, Destination{IdentityFile: "/tmp/identity"})
			}
			if !errors.Is(err, stopped) || workspacePath == "" {
				t.Fatalf("runner not invoked or error lost: %v", err)
			}
			if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
				t.Fatalf("workspace retained: %v", err)
			}
		})
	}
}

func TestDefinitionDigestCompatibility(t *testing.T) {
	s := Source{Configuration: "os", Channels: "pinned", System: "aarch64-linux"}
	legacy := sha256.Sum256([]byte(`{"Configuration":"os","Channels":"pinned","System":"aarch64-linux","Files":null}`))
	if s.Digest() != fmt.Sprintf("%x", legacy) {
		t.Fatal("omitted definitions changed the legacy digest")
	}
	for _, changed := range []Source{
		{Configuration: s.Configuration, Channels: s.Channels, System: s.System, ImageDefinition: "image"},
		{Configuration: s.Configuration, Channels: s.Channels, System: s.System, SystemDefinition: "platform"},
	} {
		if changed.Digest() == s.Digest() {
			t.Fatal("definition contents do not participate in the digest")
		}
	}
}

// These lookup-only tests need Guix's Scheme modules, but no daemon or image build.
// Set GUIX_TEST_LOOKUP_CHANNELS to also run them inside a real pinned time-machine.
func TestDefinitionLookup(t *testing.T) {
	if _, err := exec.LookPath("guix"); err != nil {
		t.Skip("Guix is required for executable definition lookup tests")
	}
	t.Setenv("GUILE_AUTO_COMPILE", "0")
	channels := ""
	if path := os.Getenv("GUIX_TEST_LOOKUP_CHANNELS"); path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		channels = string(contents)
	}
	client := Client{Runner: runnerFunc(func(ctx context.Context, dir, binary string, args ...string) (string, string, error) {
		if channels == "" {
			// Exercise exactly the pinned command body using the installed Guix.
			args = args[4:]
		}
		return (ExecRunner{}).Run(ctx, dir, binary, args...)
	})}
	for _, tc := range []struct {
		name, probe, output, failure  string
		args                          []string
		imageOnly, systemOnly, shared bool
	}{
		{name: "exported record names and built-in fallback", probe: `(use-modules (gnu image) (gnu system image) (guix platform) (guix gexp) (homelab image))
(unless (and (eq? 'homelab-raw (image-type-name (lookup-image-type-by-name 'homelab-raw)))
             (eq? 'other-raw (image-type-name (lookup-image-type-by-name 'other-raw)))
             (eq? 'efi-raw (image-type-name (lookup-image-type-by-name 'efi-raw)))
             (string=? "aarch64-unknown-linux-gnu" (platform-system->target "homelab-linux"))
             (platform? (lookup-platform-by-system "aarch64-linux"))
             (file-exists? (local-file-absolute-file-name asset)))
  (error "lookup failed"))
(display "LOOKUP-OK")`, output: "LOOKUP-OK"},
		{name: "custom system accepted during option parsing", args: []string{"--system=homelab-linux", "--help"}, output: "Usage:"},
		{name: "image definition alone", imageOnly: true, args: []string{"--list-image-types"}, output: "homelab-raw"},
		{name: "platform definition alone", systemOnly: true, args: []string{"--system=homelab-linux", "--help"}, output: "Usage:"},
		{name: "shared source with define-public loaded once", shared: true, probe: `(use-modules (gnu image) (gnu system image) (guix platform))
(unless (and (image-type? (lookup-image-type-by-name 'shared-raw))
             (platform? (lookup-platform-by-system "homelab-linux")))
  (error "shared definition not registered"))
(display "SHARED-OK")`, output: "SHARED-OK"},
		{name: "custom image constructor selected", args: []string{"image", "--system=homelab-linux", "--image-type=homelab-raw"}, failure: "CUSTOM-IMAGE-FOUND"},
		{name: "missing image name", args: []string{"image", "--image-type=missing-image"}, failure: "missing-image: no such image type"},
		{name: "unexported image name", args: []string{"image", "--image-type=private-raw"}, failure: "private-raw: no such image type"},
		{name: "missing system name", args: []string{"build", "--system=missing-system"}, failure: "'missing-system' is not a supported system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := Source{Configuration: testOperatingSystem, Channels: channels, ImageDefinition: testImageDefinition, SystemDefinition: testSystemDefinition, Files: map[string]string{
				"lookup.scm":          tc.probe,
				"asset.txt":           "supporting asset",
				"homelab/support.scm": "(define-module (homelab support) #:export (other-image-name)) (define other-image-name 'other-raw)",
			}}
			if tc.shared {
				source.SystemDefinition = `(use-modules (gnu image) (gnu system image) (guix platform))
(when (getenv "GUIX_TEST_SHARED_LOADED") (error "definition loaded twice"))
(setenv "GUIX_TEST_SHARED_LOADED" "1")
(define-public shared-image-type
  (image-type (inherit efi-raw-image-type) (name 'shared-raw)))
(define-public custom-platform
  (platform (system "homelab-linux") (target "aarch64-unknown-linux-gnu")
            (glibc-dynamic-linker "/lib/ld-linux-aarch64.so.1")))`
				source.ImageDefinition = source.SystemDefinition
			} else if tc.imageOnly {
				source.SystemDefinition = ""
			} else if tc.systemOnly {
				source.ImageDefinition = ""
			} else {
				// Platforms must be registered before dependent image source loads.
				source.ImageDefinition += "\n(lookup-platform-by-system \"homelab-linux\")"
			}
			dir, err := workspace(source)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			command, args := "system", slices.Clone(tc.args)
			if tc.probe != "" {
				command, args = "repl", []string{"--", filepath.Join(dir, "lookup.scm")}
			} else if tc.failure != "" {
				args = append(args, filepath.Join(dir, "configuration.scm"))
			}
			out, err := client.invoke(t.Context(), dir, source, command, args...)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("want error containing %q, got %v", tc.failure, err)
				}
			} else if err != nil || !strings.Contains(out, tc.output) {
				t.Fatalf("want output containing %q, got %q, error %v", tc.output, out, err)
			}
		})
	}
}
