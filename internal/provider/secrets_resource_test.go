// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	testresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func TestAccSecretsAttachment(t *testing.T) {
	client, backend := fakeClient(t)
	sourceFile := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(sourceFile, []byte("source-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	config := func(content, attachment string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "host key", private_key = "management key" }
}
resource "guix_secrets" "server" {
  secrets = %q
  file_digests = { external = filesha256(%q) }
}
resource "guix_blueprint" "server" {
  os = "system"
  channels = "pinned"
  source_files = { "public" = "public data" }
  %s
}
resource "guix_image" "server" { blueprint = guix_blueprint.server }
resource "guix_host" "server" {
  hostname = "server.local"
  blueprint = guix_blueprint.server
}
`, client.ArtifactDirectory, content, sourceFile, attachment)
	}
	var identity string
	check := func(content string, builds, provisions int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			secrets := state.RootModule().Resources["guix_secrets.server"].Primary.Attributes
			image := state.RootModule().Resources["guix_image.server"].Primary.Attributes
			if identity == "" {
				identity = secrets["id"]
			}
			digest, err := guix.Checksum(t.Context(), sourceFile)
			if err != nil {
				return err
			}
			if identity == "" || secrets["id"] != identity || image["blueprint.secrets.id"] != identity || image["blueprint.secrets.secrets"] != content || image["blueprint.secrets.file_digests.external"] != digest {
				return fmt.Errorf("secret attachment or stable identity was not preserved")
			}
			// Private descriptors must not influence the public Guix build source.
			source := guix.Source{Kind: "os", Configuration: "system", Channels: "pinned", System: "x86_64-linux", Files: map[string]string{"public": "public data"}}
			if image["input_digest"] != source.Digest() {
				return fmt.Errorf("secret content entered the Guix build source")
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.builds != builds || backend.systemBuilds != 1 || backend.deployments != 1 || backend.imageProvisions != provisions || backend.hostProvisions != provisions {
				return fmt.Errorf("builds=%d deployments=%d image provisions=%d host provisions=%d", backend.builds, backend.deployments, backend.imageProvisions, backend.hostProvisions)
			}
			if backend.installedSecrets.Expression != content {
				return fmt.Errorf("host secret was not updated")
			}
			info, err := os.Lstat(image["artifact_path"])
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return fmt.Errorf("private image permissions: %v", err)
			}
			base, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(image["artifact_path"]), "base-image"))
			if err != nil || base != image["store_path"] {
				return fmt.Errorf("clean image GC root missing: %v", err)
			}
			return nil
		}
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config("private-v1", "")},
		{Config: config("private-v1", "secrets = guix_secrets.server"), Check: check("private-v1", 2, 1)},
		{Config: config("private-v2", "secrets = guix_secrets.server"), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_image.server", plancheck.ResourceActionDestroyBeforeCreate),
		}}, Check: check("private-v2", 3, 2)},
		{Config: config("private-v2", "secrets = guix_secrets.server"), PreConfig: func() {
			if err := os.WriteFile(sourceFile, []byte("source-v2"), 0600); err != nil {
				t.Fatal(err)
			}
		}, ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_image.server", plancheck.ResourceActionDestroyBeforeCreate),
		}}, Check: check("private-v2", 4, 3)},
		{Config: config("private-v2", "secrets = guix_secrets.server"), PlanOnly: true, ExpectNonEmptyPlan: false},
		{Config: config("private-v2", ""), Check: func(state *terraform.State) error {
			if state.RootModule().Resources["guix_image.server"].Primary.Attributes["blueprint.secrets.id"] != "" {
				return fmt.Errorf("removed attachment retained")
			}
			return nil
		}},
	}})
}
