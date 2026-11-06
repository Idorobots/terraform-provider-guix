// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"fmt"
	"regexp"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	testresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func blueprintValue(t *testing.T, m BlueprintModel) types.Object {
	t.Helper()
	if m.SourceFiles.IsNull() {
		m.SourceFiles = types.MapNull(types.StringType)
	}
	value, diagnostics := types.ObjectValueFrom(t.Context(), objectAttributeTypes(t, blueprintSchema().GetType()), m)
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return value
}

func objectAttributeTypes(t *testing.T, value attr.Type) map[string]attr.Type {
	t.Helper()
	object, ok := value.(types.ObjectType)
	if !ok {
		t.Fatal("expected an object attribute type")
	}
	return object.AttrTypes
}

func testBlueprint(t *testing.T) types.Object {
	t.Helper()
	return blueprintValue(t, BlueprintModel{
		ID: types.StringValue("blueprint-id"), OS: types.StringValue("blueprint OS"),
		Channels:    types.StringValue("blueprint channels"),
		SourceFiles: types.MapValueMust(types.StringType, map[string]attr.Value{"asset": types.StringValue("public asset")}),
		Secrets:     types.StringNull(),
	})
}

func TestBlueprintSourceSelection(t *testing.T) {
	for _, preferred := range []string{"image", "machine"} {
		for _, tc := range []struct {
			name           string
			os, selected   types.String
			kind           string
			known, invalid bool
		}{
			{"fallback", types.StringValue("OS"), types.StringNull(), "os", true, false},
			{"preferred", types.StringValue("OS"), types.StringValue("preferred"), preferred, true, false},
			{"unknown preferred", types.StringValue("OS"), types.StringUnknown(), preferred, false, false},
			{"unknown OS", types.StringUnknown(), types.StringNull(), "os", false, false},
			{"missing", types.StringNull(), types.StringNull(), "os", false, true},
		} {
			t.Run(preferred+"/"+tc.name, func(t *testing.T) {
				m := BlueprintModel{OS: tc.os, Channels: types.StringValue("pinned"), Secrets: types.StringNull()}
				if preferred == "image" {
					m.Image = tc.selected
				} else {
					m.Machine = tc.selected
				}
				s, known, diagnostics := blueprintSource(t.Context(), blueprintValue(t, m), types.StringValue("aarch64-linux"), preferred)
				if known != tc.known || diagnostics.HasError() != tc.invalid || s.Kind != tc.kind {
					t.Fatalf("selection kind=%s known=%t diagnostics=%v", s.Kind, known, diagnostics)
				}
				if known && (s.System != "aarch64-linux" || s.Configuration == "") {
					t.Fatal("effective source lost inputs")
				}
			})
		}
	}
}

func TestHostBlueprintCommissioningInfoLog(t *testing.T) {
	client, backend := fakeClient(t)
	r := &HostResource{client: client}
	state := hostState(t, r)
	var model HostModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	client.SSH = types.ObjectValueMust(objectAttributeTypes(t, sshSchema().GetType()), map[string]attr.Value{
		"host_key": types.StringValue("fake host key"), "private_key": types.StringValue("fake management key"),
		"identity_file": types.StringNull(), "user": types.StringNull(), "port": types.Int64Null(),
	})
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	if diagnostics := r.reconcile(ctx, &model, "create"); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	want := guix.Source{Kind: "os", Configuration: "blueprint OS", Channels: "blueprint channels", System: "aarch64-linux", Files: map[string]string{"asset": "public asset"}}
	if backend.observed != desired(want) {
		t.Fatalf("deployed %s; want %s", backend.observed, desired(want))
	}
	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry["@message"] == "Waiting for manually commissioned Guix host" && entry["@level"] == "info" && entry["instructions"] != "" && entry["image_path"] == nil {
			return
		}
	}
	t.Fatal("missing independent host commissioning INFO log")
}

func TestAccBlueprintLeaves(t *testing.T) {
	client, backend := fakeClient(t)
	config := func(os, image, machine, channels, asset, hostSystem string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "host key", private_key = "management key" }
}
resource "guix_blueprint" "server" {
  os = %q
  image = %s
  machine = %s
  channels = %q
  source_files = { asset = %q }
}
resource "guix_image" "server" { blueprint = guix_blueprint.server }
resource "guix_host" "server" {
  hostname = "server.local"
  system = %q
  blueprint = guix_blueprint.server
}
`, client.ArtifactDirectory, os, image, machine, channels, asset, hostSystem)
	}
	var blueprintID, hostID string
	check := func(builds, deployments int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			bp := state.RootModule().Resources["guix_blueprint.server"].Primary.ID
			host := state.RootModule().Resources["guix_host.server"].Primary.ID
			if blueprintID == "" {
				blueprintID, hostID = bp, host
			}
			if bp != blueprintID || host != hostID {
				return fmt.Errorf("descriptor or host identity changed")
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.builds != builds || backend.deployments != deployments {
				return fmt.Errorf("builds=%d deployments=%d; want %d %d", backend.builds, backend.deployments, builds, deployments)
			}
			return nil
		}
	}
	steps := []testresource.TestStep{
		{Config: config("os-v1", "null", "null", "channels-v1", "asset-v1", "x86_64-linux"), Check: check(1, 1)},
		{Config: config("os-v1", `"image-v1"`, "null", "channels-v1", "asset-v1", "x86_64-linux"), Check: check(2, 1)},
		{Config: config("os-v1", `"image-v1"`, `"machine-v1"`, "channels-v1", "asset-v1", "x86_64-linux"), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("guix_image.server", plancheck.ResourceActionUpdate)}}, Check: check(2, 2)},
		{Config: config("unused-os", `"image-v1"`, `"machine-v1"`, "channels-v1", "asset-v1", "x86_64-linux"), Check: check(2, 2)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v1"`, "channels-v1", "asset-v1", "x86_64-linux"), Check: check(3, 2)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v2"`, "channels-v1", "asset-v1", "x86_64-linux"), Check: check(3, 3)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v2"`, "channels-v1", "asset-v2", "x86_64-linux"), Check: check(4, 4)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v2"`, "channels-v2", "asset-v2", "x86_64-linux"), Check: check(5, 5)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v2"`, "channels-v2", "asset-v2", "aarch64-linux"), Check: check(5, 6)},
		{Config: config("unused-os", `"image-v2"`, `"machine-v2"`, "channels-v2", "asset-v2", "aarch64-linux"), PlanOnly: true, ExpectNonEmptyPlan: false},
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: steps})
}

func TestAccBlueprintMissingSpecifications(t *testing.T) {
	client, _ := fakeClient(t)
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{{Config: `resource "guix_blueprint" "empty" { channels = "pinned" }`}}})
	for _, leaf := range []string{"image", "host"} {
		t.Run(leaf, func(t *testing.T) {
			option := ""
			if leaf == "host" {
				option = `hostname = "server.local"`
			}
			other := "machine"
			if leaf == "host" {
				other = "image"
			}
			for _, extra := range []string{"", other + ` = "wrong consumer"`} {
				testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{{Config: fmt.Sprintf(`
resource "guix_blueprint" "empty" {
  channels = "pinned"
  %s
}
resource "guix_%s" "invalid" {
  blueprint = guix_blueprint.empty
  %s
}`, extra, leaf, option), ExpectError: regexp.MustCompile("Missing Guix specification")}}})
			}
		})
	}
}

func TestAccBlueprintReservedSource(t *testing.T) {
	client, _ := fakeClient(t)
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{{
		Config: `resource "guix_blueprint" "invalid" {
  channels = "pinned"
  source_files = { "specification.scm" = "reserved" }
}`,
		ExpectError: regexp.MustCompile("Invalid source file"),
	}}})
}
