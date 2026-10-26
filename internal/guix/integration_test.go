// SPDX-License-Identifier: MPL-2.0

package guix_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/SpartanWorks/terraform-provider-guix/internal/remote"
)

// TestIntegrationGuix is explicitly opt-in because it performs real builds and,
// if GUIX_TEST_DEPLOY_ADDRESS is supplied, reconfigures that running Guix host.
//
// GUIX_INTEGRATION=1 GUIX_TEST_CONFIGURATION=/path/to/system.scm \
// GUIX_TEST_CHANNELS=/path/to/channels-lock.scm go test -v ./internal/guix -run Integration
//
// Optional: GUIX_TEST_SOURCE_FILES is a JSON file mapping relative names to source
// contents. GUIX_TEST_SYSTEM and GUIX_TEST_IMAGE_TYPE default to x86_64-linux and
// efi-raw. For real deployment, also set GUIX_TEST_DEPLOY_ADDRESS,
// GUIX_TEST_HOST_KEY, and GUIX_TEST_IDENTITY_FILE (root SSH on port 22).
// GUIX_TEST_IMAGE_DEFINITION and GUIX_TEST_SYSTEM_DEFINITION optionally name
// Scheme files whose contents are supplied as custom image/platform definitions.
func TestIntegrationGuix(t *testing.T) {
	if os.Getenv("GUIX_INTEGRATION") != "1" {
		t.Skip("set GUIX_INTEGRATION=1 to run real Guix builds")
	}
	read := func(name string) string {
		t.Helper()
		path := os.Getenv(name)
		if path == "" {
			t.Fatalf("%s is required", name)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}
	source := guix.Source{Configuration: read("GUIX_TEST_CONFIGURATION"), Channels: read("GUIX_TEST_CHANNELS"), System: os.Getenv("GUIX_TEST_SYSTEM")}
	if os.Getenv("GUIX_TEST_IMAGE_DEFINITION") != "" {
		source.ImageDefinition = read("GUIX_TEST_IMAGE_DEFINITION")
	}
	if os.Getenv("GUIX_TEST_SYSTEM_DEFINITION") != "" {
		source.SystemDefinition = read("GUIX_TEST_SYSTEM_DEFINITION")
	}
	if source.System == "" {
		source.System = "x86_64-linux"
	}
	if os.Getenv("GUIX_TEST_SOURCE_FILES") != "" {
		if err := json.Unmarshal([]byte(read("GUIX_TEST_SOURCE_FILES")), &source.Files); err != nil {
			t.Fatal(err)
		}
	}
	imageType := os.Getenv("GUIX_TEST_IMAGE_TYPE")
	if imageType == "" {
		imageType = "efi-raw"
	}
	client := guix.Client{Runner: guix.ExecRunner{}}
	root := filepath.Join(t.TempDir(), "image")
	store, checksum, err := client.BuildImage(t.Context(), source, imageType, root)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err != nil || resolved != store {
		t.Fatalf("GC root=%s store=%s err=%v", resolved, store, err)
	}
	if len(checksum) != 64 {
		t.Fatalf("invalid checksum: %s", checksum)
	}
	// Image definitions are needed only to construct the image. Hosts inherit
	// platform definitions for subsequent system builds and deployments.
	source.ImageDefinition = ""
	expected, err := client.BuildSystem(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("image=%s checksum=%s system=%s", store, checksum, expected)
	if address := os.Getenv("GUIX_TEST_DEPLOY_ADDRESS"); address != "" {
		d := guix.Destination{Address: address, Port: 22, User: "root", HostKey: os.Getenv("GUIX_TEST_HOST_KEY"), IdentityFile: os.Getenv("GUIX_TEST_IDENTITY_FILE")}
		if d.HostKey == "" || d.IdentityFile == "" {
			t.Fatal("GUIX_TEST_HOST_KEY and GUIX_TEST_IDENTITY_FILE are required for deployment")
		}
		if _, err := (remote.SSH{}).Inspect(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if err := client.Deploy(t.Context(), source, d); err != nil {
			t.Fatal(err)
		}
		actual, err := (remote.SSH{}).Inspect(t.Context(), d)
		if err != nil || actual != expected {
			t.Fatalf("actual=%s expected=%s err=%v", actual, expected, err)
		}
	}
}
