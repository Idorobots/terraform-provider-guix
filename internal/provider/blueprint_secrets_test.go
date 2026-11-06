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

func TestAccBlueprintSecretsAndNativeFileTrigger(t *testing.T) {
	client, backend := fakeClient(t)
	sourceFile := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(sourceFile, []byte("source-v1"), 0600); err != nil {
		t.Fatal(err)
	}
	config := func(expression string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "host key", private_key = "management key" }
}
resource "terraform_data" "secret_files" {
  input = { identity = filesha256(%q) }
}
resource "guix_blueprint" "server" {
  os = "system"
  channels = "pinned"
  source_files = { "public" = "public data" }
  secrets = %s
}
resource "guix_image" "server" {
  blueprint = guix_blueprint.server
  lifecycle { replace_triggered_by = [terraform_data.secret_files] }
}
resource "guix_host" "server" {
  hostname = "server.local"
  blueprint = guix_blueprint.server
}
`, client.ArtifactDirectory, sourceFile, expression)
	}
	check := func(expression string, builds, imageProvisions, hostProvisions int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			image := state.RootModule().Resources["guix_image.server"].Primary.Attributes
			if image["blueprint.secrets"] != expression {
				return fmt.Errorf("private Scheme expression was not preserved")
			}
			source := guix.Source{Kind: "os", Configuration: "system", Channels: "pinned", System: "x86_64-linux", Files: map[string]string{"public": "public data"}}
			if image["input_digest"] != source.Digest() {
				return fmt.Errorf("private inputs entered the public Guix build source")
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.builds != builds || backend.systemBuilds != 1 || backend.deployments != 1 || backend.imageProvisions != imageProvisions || backend.hostProvisions != hostProvisions {
				return fmt.Errorf("builds=%d system builds=%d deployments=%d image provisions=%d host provisions=%d", backend.builds, backend.systemBuilds, backend.deployments, backend.imageProvisions, backend.hostProvisions)
			}
			if backend.installedSecrets.Expression != expression {
				return fmt.Errorf("host secret declaration was not updated")
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
		{Config: config("null")},
		{Config: config(`"private-v1"`), Check: check("private-v1", 2, 1, 1)},
		{Config: config(`"private-v2"`), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_image.server", plancheck.ResourceActionDestroyBeforeCreate),
		}}, Check: check("private-v2", 3, 2, 2)},
		{Config: config(`"private-v2"`), PreConfig: func() {
			if err := os.WriteFile(sourceFile, []byte("source-v2"), 0600); err != nil {
				t.Fatal(err)
			}
		}, ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_image.server", plancheck.ResourceActionDestroyBeforeCreate),
			plancheck.ExpectResourceAction("guix_host.server", plancheck.ResourceActionNoop),
		}}, Check: check("private-v2", 4, 3, 2)},
		{Config: config(`"private-v2"`), PlanOnly: true, ExpectNonEmptyPlan: false},
		{Config: config("null"), Check: func(state *terraform.State) error {
			if state.RootModule().Resources["guix_image.server"].Primary.Attributes["blueprint.secrets"] != "" {
				return fmt.Errorf("removed secrets declaration retained")
			}
			return nil
		}},
	}})
}
