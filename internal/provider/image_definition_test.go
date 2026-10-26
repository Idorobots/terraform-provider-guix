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

func TestAccImageDefinitions(t *testing.T) {
	client, backend := fakeClient(t)
	dir := t.TempDir()
	for _, name := range []string{"image-v1", "image-v2", "platform-v1", "platform-v2"} {
		if err := os.WriteFile(filepath.Join(dir, name+".scm"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := func(image, platform string) string {
		definitions := ""
		if image != "" {
			definitions += fmt.Sprintf("image_definition = file(%q)\n", filepath.Join(dir, image+".scm"))
		}
		if platform != "" {
			definitions += fmt.Sprintf("system_definition = file(%q)\n", filepath.Join(dir, platform+".scm"))
		}
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "fake host key", private_key = "fake management key" }
}
resource "guix_image" "custom" {
  configuration = "os"
  channels = "pinned"
  %s
}
resource "guix_host" "custom" {
  hostname = "custom.local"
  image = guix_image.custom
}
`, client.ArtifactDirectory, definitions)
	}
	check := func(image, platform string, builds, deployments int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			backend.mu.Lock()
			defer backend.mu.Unlock()
			want := guix.Source{Configuration: "os", Channels: "pinned", System: "x86_64-linux", ImageDefinition: image, SystemDefinition: platform}
			if backend.imageSource.Digest() != want.Digest() || backend.builds != builds || backend.deployments != deployments {
				return fmt.Errorf("unexpected image source/builds/deployments: %+v / %d / %d", backend.imageSource, backend.builds, backend.deployments)
			}
			imageState := state.RootModule().Resources["guix_image.custom"].Primary.Attributes
			if imageState["input_digest"] != want.Digest() {
				return fmt.Errorf("image digest omitted custom definitions: %s", imageState["input_digest"])
			}
			want.ImageDefinition = ""
			host := state.RootModule().Resources["guix_host.custom"].Primary.Attributes
			if host["image.system_definition"] != platform || host["deployed_system"] != desired(want) {
				return fmt.Errorf("host did not inherit platform definition: %v", host)
			}
			return nil
		}
	}
	replace := testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction("guix_image.custom", plancheck.ResourceActionDestroyBeforeCreate),
		plancheck.ExpectResourceAction("guix_host.custom", plancheck.ResourceActionUpdate),
	}}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config("image-v1", "platform-v1"), Check: check("image-v1", "platform-v1", 1, 1)},
		{Config: config("image-v2", "platform-v1"), ConfigPlanChecks: replace, Check: check("image-v2", "platform-v1", 2, 1)},
		{Config: config("image-v2", "platform-v2"), ConfigPlanChecks: replace, Check: check("image-v2", "platform-v2", 3, 2)},
		{Config: config("", ""), ConfigPlanChecks: replace, Check: check("", "", 4, 3)},
	}})
}
