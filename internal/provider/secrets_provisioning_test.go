// SPDX-License-Identifier: MPL-2.0
package provider

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

func privateBlueprint(t *testing.T, content, digest types.String) types.Object {
	t.Helper()
	var blueprint BlueprintModel
	if d := testBlueprint(t).As(t.Context(), &blueprint, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	digests := types.MapValueMust(types.StringType, map[string]attr.Value{"key": digest})
	value, d := types.ObjectValueFrom(t.Context(), objectAttributeTypes(t, blueprintSecretsSchema().GetType()), SecretsModel{ID: types.StringUnknown(), Secrets: content, FileDigests: digests})
	if d.HasError() {
		t.Fatal(d)
	}
	blueprint.Secrets = value
	return blueprintValue(t, blueprint)
}

func TestPrivateInputsKnownAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name            string
		content, digest types.String
		known           bool
	}{
		{"known expression with unknown descriptor identity", types.StringValue("private"), types.StringValue("digest"), true},
		{"unknown expression", types.StringUnknown(), types.StringValue("digest"), false},
		{"unknown digest", types.StringValue("private"), types.StringUnknown(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, known, d := blueprintSecrets(t.Context(), privateBlueprint(t, tc.content, tc.digest))
			if d.HasError() || known != tc.known {
				t.Fatalf("known=%t diagnostics=%v", known, d)
			}
			if known && files.Expression != "private" {
				t.Fatal("payload lost")
			}
		})
	}
}

func TestHostSecretOnlyReconciliation(t *testing.T) {
	client, backend := fakeClient(t)
	client.SSH = types.ObjectValueMust(objectAttributeTypes(t, sshSchema().GetType()), map[string]attr.Value{
		"host_key": types.StringValue("host key"), "private_key": types.StringValue("management key"),
		"identity_file": types.StringNull(), "user": types.StringNull(), "port": types.Int64Null(),
	})
	r := &HostResource{client: client}
	state := hostState(t, r)
	var old HostModel
	if d := state.Get(t.Context(), &old); d.HasError() {
		t.Fatal(d)
	}
	old.Blueprint = privateBlueprint(t, types.StringValue("v1"), types.StringValue("0600"))
	s, _, _ := blueprintSource(t.Context(), old.Blueprint, old.System, "machine")
	backend.observed = desired(s)
	if d := r.reconcile(t.Context(), &old, "create"); d.HasError() {
		t.Fatal(d)
	}
	if backend.hostProvisions != 1 || backend.deployments != 0 {
		t.Fatal("already-matching system skipped provisioning or was reconfigured")
	}
	if d := state.Set(t.Context(), &old); d.HasError() {
		t.Fatal(d)
	}
	for i, mode := range []string{"0640", "0600"} {
		plan := tfsdk.Plan(state)
		if d := plan.SetAttribute(t.Context(), path.Root("blueprint"), privateBlueprint(t, types.StringValue("v2"), types.StringValue(mode))); d.HasError() {
			t.Fatal(d)
		}
		planned := resource.ModifyPlanResponse{Plan: plan}
		r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Config: tfsdk.Config(plan), Plan: plan, State: state}, &planned)
		var next HostModel
		if d := planned.Plan.Get(t.Context(), &next); d.HasError() {
			t.Fatal(d)
		}
		if !next.IPv4.Equal(old.IPv4) {
			t.Fatal("secret-only change invalidated dependent image IP input")
		}
		response := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{Plan: planned.Plan, State: state}, &response)
		if response.Diagnostics.HasError() {
			t.Fatal(response.Diagnostics)
		}
		state = response.State
		if backend.systemBuilds != 1 || backend.deployments != 0 || backend.hostProvisions != i+2 || backend.installedSecrets.FileDigests["key"] != mode {
			t.Fatal("secret-only update rebuilt/reconfigured or lost its digest")
		}
	}
}

func TestProvisioningFailurePreventsDeployment(t *testing.T) {
	client, backend := fakeClient(t)
	client.SSH = types.ObjectValueMust(objectAttributeTypes(t, sshSchema().GetType()), map[string]attr.Value{
		"host_key": types.StringValue("host key"), "private_key": types.StringValue("management key"),
		"identity_file": types.StringNull(), "user": types.StringNull(), "port": types.Int64Null(),
	})
	r := &HostResource{client: client}
	state := hostState(t, r)
	var model HostModel
	if d := state.Get(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	model.Blueprint = privateBlueprint(t, types.StringValue("private"), types.StringValue("0600"))
	backend.provisionError = errors.New("installation failed")
	previous := model.DeployedSystem
	if d := r.reconcile(t.Context(), &model, "create"); !d.HasError() {
		t.Fatal("provisioning failure was ignored")
	}
	if backend.deployments != 0 || !model.DeployedSystem.Equal(previous) {
		t.Fatal("deployment/state advanced after failed provisioning")
	}
}

func TestImageProvisioningFailureReleasesRoot(t *testing.T) {
	client, backend := fakeClient(t)
	backend.provisionError = errors.New("authentication cancelled")
	r := &ImageResource{client: client}
	var response resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &response)
	plan := tfsdk.Plan{Schema: response.Schema}
	model := ImageModel{Name: types.StringValue("test"), Blueprint: privateBlueprint(t, types.StringValue("private"), types.StringValue("0600")), System: types.StringValue("x86_64-linux"), BuildTimeout: types.StringValue("1m")}
	if d := plan.Set(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	created := resource.CreateResponse{State: tfsdk.State{Schema: response.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &created)
	if !created.Diagnostics.HasError() {
		t.Fatal("expected provisioning failure")
	}
	entries, err := os.ReadDir(filepath.Join(client.ArtifactDirectory, "test"))
	if err != nil || len(entries) != 0 {
		t.Fatal("failed image left an owned artifact/GC root")
	}
}

func TestMissingExtensionFailsBeforeBuildOrContact(t *testing.T) {
	client, backend := fakeClient(t)
	backend.extensionError = errors.New("install guix secrets in blueprint.channels")
	r := &ImageResource{client: client}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	plan := tfsdk.Plan{Schema: schema.Schema}
	model := ImageModel{Name: types.StringValue("test"), Blueprint: privateBlueprint(t, types.StringValue("private"), types.StringValue("digest")), System: types.StringValue("x86_64-linux"), BuildTimeout: types.StringValue("1m")}
	if d := plan.Set(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	created := resource.CreateResponse{State: tfsdk.State{Schema: schema.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &created)
	if !created.Diagnostics.HasError() || backend.builds != 0 || backend.imageProvisions != 0 {
		t.Fatal("missing extension did not fail before build")
	}
	client.SSH = types.ObjectValueMust(objectAttributeTypes(t, sshSchema().GetType()), map[string]attr.Value{
		"host_key": types.StringValue("host key"), "private_key": types.StringValue("management key"),
		"identity_file": types.StringNull(), "user": types.StringNull(), "port": types.Int64Null(),
	})
	host := &HostResource{client: client}
	state := hostState(t, host)
	var machine HostModel
	if d := state.Get(t.Context(), &machine); d.HasError() {
		t.Fatal(d)
	}
	machine.Blueprint = model.Blueprint
	if d := host.reconcile(t.Context(), &machine, "create"); !d.HasError() || backend.attempts != 0 || backend.systemBuilds != 0 {
		t.Fatal("missing extension did not fail before host contact/build")
	}
}

func TestMissingPrivateImageReleasesBaseRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "11111111111111111111111111111111", "image")
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(filepath.Dir(root), "base-image")
	if err := os.Symlink("/clean-image", base); err != nil {
		t.Fatal(err)
	}
	model := ImageModel{ID: types.StringValue("11111111111111111111111111111111"), ArtifactPath: types.StringValue(root), StorePath: types.StringValue("/clean-image"), Blueprint: privateBlueprint(t, types.StringValue("private"), types.StringValue("digest"))}
	if err := releaseImage(model); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphaned base GC root")
	}
}
