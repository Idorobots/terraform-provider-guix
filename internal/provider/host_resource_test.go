// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestHostCommissioningPlanWarning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pending    bool
		create     bool
		destroy    bool
		wantDetail string
	}{
		{"known blueprint", false, true, false, "reconciles its blueprint"},
		{"pending blueprint", true, true, false, "reconciles its blueprint"},
		{"existing host", false, false, false, ""},
		{"destroy", false, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &HostResource{}
			state := hostState(t, r)
			plan := tfsdk.Plan(state)
			if tc.pending {
				if diagnostics := plan.SetAttribute(t.Context(), path.Root("blueprint"), types.ObjectUnknown(blueprintSchema().GetType().(types.ObjectType).AttrTypes)); diagnostics.HasError() {
					t.Fatal(diagnostics)
				}
			}
			// Keep observations equal so an existing host has no drift to reconcile.
			if diagnostics := plan.SetAttribute(t.Context(), path.Root("system_path"), types.StringValue("desired")); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			state.Raw = plan.Raw
			if tc.create {
				state.RemoveResource(t.Context())
			}
			if tc.destroy {
				plan.Raw = tftypes.NewValue(plan.Raw.Type(), nil)
			}
			response := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Config: tfsdk.Config(plan), Plan: plan, State: state}, &response)
			if tc.wantDetail == "" {
				if len(response.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics: %v", response.Diagnostics)
				}
				return
			}
			if len(response.Diagnostics) != 1 || response.Diagnostics[0].Severity() != diag.SeverityWarning {
				t.Fatalf("expected one commissioning warning: %v", response.Diagnostics)
			}
			warning := response.Diagnostics[0]
			if warning.Summary() != "Manual commissioning required" {
				t.Fatalf("unexpected summary: %s", warning.Summary())
			}
			for _, text := range []string{tc.wantDetail, "Boot a Guix System", "node.local", "mDNS", "SSH", "create timeout"} {
				if !strings.Contains(warning.Detail(), text) {
					t.Fatalf("warning missing %q: %s", text, warning.Detail())
				}
			}
		})
	}
}

func TestLeafBlueprintSchemas(t *testing.T) {
	for _, r := range []resource.Resource{&HostResource{}, &ImageResource{}} {
		var response resource.SchemaResponse
		r.Schema(t.Context(), resource.SchemaRequest{}, &response)
		if !response.Schema.Attributes["blueprint"].IsRequired() {
			t.Fatal("blueprint must be required")
		}
		if response.Schema.Attributes["system"] == nil {
			t.Fatal("leaf architecture is missing")
		}
		for _, name := range []string{"image", "configuration", "channels", "source_files", "secrets", "bootstrap_image", "bootstrap_image_path"} {
			if _, exists := response.Schema.Attributes[name]; exists {
				t.Fatalf("legacy host input %q is still present", name)
			}
		}
	}
}
