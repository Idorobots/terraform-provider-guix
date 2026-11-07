// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

type planResolver struct {
	address string
	err     error
	calls   int
}

func (r *planResolver) Resolve(ctx context.Context, hostname string) (string, error) {
	r.calls++
	if hostname != "node.local" {
		return "", errors.New("unexpected hostname")
	}
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("missing discovery deadline")
	}
	return r.address, r.err
}

func TestHostCommissioningPlanWarning(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pending         bool
		create          bool
		destroy         bool
		unknownHostname bool
		unconfigured    bool
		address         string
		resolveError    error
		wantSummary     string
		wantCalls       int
	}{
		{name: "discoverable", create: true, address: "192.0.2.1", wantCalls: 1},
		{name: "discoverable with pending blueprint", create: true, pending: true, address: "192.0.2.1", wantCalls: 1},
		{name: "unavailable", create: true, resolveError: discovery.ErrUnavailable, wantSummary: "Manual commissioning required", wantCalls: 1},
		{name: "discovery timeout", create: true, resolveError: context.DeadlineExceeded, wantSummary: "Host discovery unavailable", wantCalls: 1},
		{name: "discovery failure", create: true, resolveError: errors.New("Avahi is not installed"), wantSummary: "Host discovery unavailable", wantCalls: 1},
		{name: "empty address", create: true, wantSummary: "Manual commissioning required", wantCalls: 1},
		{name: "unknown hostname", create: true, unknownHostname: true, wantSummary: "Manual commissioning required"},
		{name: "unconfigured provider", create: true, unconfigured: true, wantSummary: "Manual commissioning required"},
		{name: "existing host", address: "192.0.2.1"},
		{name: "destroy", destroy: true, address: "192.0.2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &HostResource{}
			resolver := &planResolver{address: tc.address, err: tc.resolveError}
			if !tc.unconfigured {
				r.client = &Client{Resolver: resolver}
			}
			state := hostState(t, r)
			plan := tfsdk.Plan(state)
			if tc.unknownHostname {
				if diagnostics := plan.SetAttribute(t.Context(), path.Root("hostname"), types.StringUnknown()); diagnostics.HasError() {
					t.Fatal(diagnostics)
				}
			}
			if tc.pending {
				if diagnostics := plan.SetAttribute(t.Context(), path.Root("blueprint"), types.ObjectUnknown(objectAttributeTypes(t, blueprintSchema().GetType()))); diagnostics.HasError() {
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
			if resolver.calls != tc.wantCalls {
				t.Fatalf("discovery calls = %d, want %d", resolver.calls, tc.wantCalls)
			}
			if tc.wantSummary == "" {
				if len(response.Diagnostics) != 0 {
					t.Fatalf("unexpected diagnostics: %v", response.Diagnostics)
				}
				return
			}
			if len(response.Diagnostics) != 1 || response.Diagnostics[0].Severity() != diag.SeverityWarning {
				t.Fatalf("expected one commissioning warning: %v", response.Diagnostics)
			}
			warning := response.Diagnostics[0]
			if warning.Summary() != tc.wantSummary {
				t.Fatalf("unexpected summary: %s", warning.Summary())
			}
			if tc.wantSummary != "Manual commissioning required" {
				if !strings.Contains(warning.Detail(), "Host discovery will be retried during apply") {
					t.Fatalf("missing discovery failure details: %s", warning.Detail())
				}
				return
			}
			for _, text := range []string{"reconciles its blueprint", "boot a Guix System", "mDNS", "SSH", "create timeout"} {
				if !strings.Contains(warning.Detail(), text) {
					t.Fatalf("warning missing %q: %s", text, warning.Detail())
				}
			}
			if !tc.unknownHostname && !strings.Contains(warning.Detail(), "node.local") {
				t.Fatalf("warning missing hostname: %s", warning.Detail())
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
