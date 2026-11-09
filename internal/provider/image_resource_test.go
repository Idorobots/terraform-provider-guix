// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	testresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func imageTimeoutValues(values map[string]string) types.Object {
	attributes := map[string]attr.Type{"create": types.StringType, "read": types.StringType, "update": types.StringType}
	if values == nil {
		return types.ObjectNull(attributes)
	}
	result := map[string]attr.Value{}
	for name := range attributes {
		result[name] = types.StringNull()
		if value, ok := values[name]; ok {
			result[name] = types.StringValue(value)
		}
	}
	return types.ObjectValueMust(attributes, result)
}

func TestImageTimeouts(t *testing.T) {
	for operation, fallback := range map[string]time.Duration{"create": 2 * time.Hour, "update": 2 * time.Hour, "read": 30 * time.Second} {
		for _, values := range []map[string]string{nil, {}, {operation: "7m"}, {operation: "0s"}, {operation: "-1m"}, {operation: "invalid"}} {
			duration, diagnostics := imageTimeout(t.Context(), imageTimeoutValues(values), operation)
			value := values[operation]
			invalid := value != "" && value != "7m"
			if diagnostics.HasError() != invalid {
				t.Fatalf("%s=%q: %v", operation, value, diagnostics)
			}
			if !invalid && ((value == "" && duration != fallback) || (value == "7m" && duration != 7*time.Minute)) {
				t.Fatalf("%s=%q: duration %v", operation, value, duration)
			}
		}
	}
}

type deadlineImageBackend struct {
	*fakeBackend
	deadline time.Duration
	fail     bool
}

func (backend *deadlineImageBackend) BuildImage(ctx context.Context, source guix.Source, imageType, target, root string) (string, string, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "", "", errors.New("missing operation deadline")
	}
	backend.deadline = time.Until(deadline)
	if backend.fail {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	return backend.fakeBackend.BuildImage(ctx, source, imageType, target, root)
}

func createTestImage(t *testing.T, r *ImageResource, private bool) tfsdk.State {
	t.Helper()
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	model := ImageModel{Name: types.StringValue("test"), Blueprint: testBlueprint(t), System: types.StringValue("x86_64-linux"), Timeouts: imageTimeoutValues(map[string]string{"create": "1m", "update": "5ms", "read": "1m"})}
	if private {
		model.Blueprint = privateBlueprint(t, types.StringValue("private"))
	}
	plan := tfsdk.Plan{Schema: schema.Schema}
	if d := plan.Set(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: schema.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	return response.State
}

func TestImageRebuildDeadlinesAndRecovery(t *testing.T) {
	client, backend := fakeClient(t)
	deadlines := &deadlineImageBackend{fakeBackend: backend}
	client.Guix = deadlines
	r := &ImageResource{client: client}
	state := createTestImage(t, r, false)
	if deadlines.deadline < 50*time.Second || deadlines.deadline > time.Minute {
		t.Fatalf("create used wrong deadline: %v", deadlines.deadline)
	}
	var old ImageModel
	if d := state.Get(t.Context(), &old); d.HasError() {
		t.Fatal(d)
	}
	plan := tfsdk.Plan(state)
	if d := plan.SetAttribute(t.Context(), path.Root("target"), types.StringValue("aarch64-linux-gnu")); d.HasError() {
		t.Fatal(d)
	}
	deadlines.fail = true
	failed := resource.UpdateResponse{State: state}
	r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &failed)
	if !failed.Diagnostics.HasError() || deadlines.deadline > 5*time.Millisecond || !failed.State.Raw.Equal(state.Raw) {
		t.Fatalf("update did not enforce its deadline or changed prior state: %v", failed.Diagnostics)
	}
	if _, err := os.Stat(old.ArtifactPath.ValueString()); err != nil {
		t.Fatalf("failed rebuild discarded the previous image: %v", err)
	}
	deadlines.fail = false
	if d := plan.SetAttribute(t.Context(), path.Root("timeouts"), imageTimeoutValues(map[string]string{"create": "1ns", "update": "1m"})); d.HasError() {
		t.Fatal(d)
	}
	succeeded := resource.UpdateResponse{State: state}
	r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &succeeded)
	if succeeded.Diagnostics.HasError() {
		t.Fatal(succeeded.Diagnostics)
	}
	var next ImageModel
	if d := succeeded.State.Get(t.Context(), &next); d.HasError() {
		t.Fatal(d)
	}
	if !next.ID.Equal(old.ID) || next.ArtifactPath.Equal(old.ArtifactPath) || deadlines.deadline < 50*time.Second {
		t.Fatal("rebuild lost resource identity, reused the old artifact or used create's deadline")
	}
	if _, err := os.Lstat(old.ArtifactPath.ValueString()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful rebuild retained the previous artifact")
	}
	if missing, err := checkImage(next); missing || err != nil {
		t.Fatalf("rebuilt image is invalid: %v", err)
	}
	if err := releaseImage(next); err != nil {
		t.Fatal(err)
	}
}

func TestImageRefreshChecksExistenceWithoutChecksum(t *testing.T) {
	client, backend := fakeClient(t)
	r := &ImageResource{client: client}
	state := createTestImage(t, r, true)
	var model ImageModel
	if d := state.Get(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	if err := os.WriteFile(model.ArtifactPath.ValueString(), []byte("changed bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	response := resource.ReadResponse{State: state}
	r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() || response.State.Raw.IsNull() || backend.builds != 1 {
		t.Fatalf("refresh hashed or rebuilt the image: %v", response.Diagnostics)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &canceled)
	if !canceled.Diagnostics.HasError() || !canceled.State.Raw.Equal(state.Raw) {
		t.Fatal("canceled refresh lost state or ignored cancellation")
	}
	expiredState := state
	if d := expiredState.SetAttribute(t.Context(), path.Root("timeouts"), imageTimeoutValues(map[string]string{"read": "1ns"})); d.HasError() {
		t.Fatal(d)
	}
	expired := resource.ReadResponse{State: expiredState}
	r.Read(t.Context(), resource.ReadRequest{State: expiredState}, &expired)
	if !expired.Diagnostics.HasError() || !expired.State.Raw.Equal(expiredState.Raw) {
		t.Fatal("read deadline was ignored or removed a valid image from state")
	}
	if err := os.Remove(model.ArtifactPath.ValueString()); err != nil {
		t.Fatal(err)
	}
	missing := resource.ReadResponse{State: state}
	r.Read(t.Context(), resource.ReadRequest{State: state}, &missing)
	if missing.Diagnostics.HasError() || !missing.State.Raw.IsNull() {
		t.Fatalf("missing image was not removed from state: %v", missing.Diagnostics)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(model.ArtifactPath.ValueString()), "base-image")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing image retained its GC root")
	}
}

func TestImageInitialBuildDeadline(t *testing.T) {
	client, backend := fakeClient(t)
	deadlines := &deadlineImageBackend{fakeBackend: backend, fail: true}
	client.Guix = deadlines
	r := &ImageResource{client: client}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	model := ImageModel{Name: types.StringValue("test"), Blueprint: testBlueprint(t), System: types.StringValue("x86_64-linux"), Timeouts: imageTimeoutValues(map[string]string{"create": "5ms", "update": "1m"})}
	plan := tfsdk.Plan{Schema: schema.Schema}
	if d := plan.Set(t.Context(), &model); d.HasError() {
		t.Fatal(d)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: schema.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
	if !response.Diagnostics.HasError() || deadlines.deadline > 5*time.Millisecond {
		t.Fatal("create did not use its own deadline")
	}
	files, err := filepath.Glob(filepath.Join(client.ArtifactDirectory, "test", "*", "*"))
	if err != nil || len(files) != 0 {
		t.Fatal("failed create retained partial artifact references")
	}
}

func TestImageFailedReprovisionRetainsPreviousArtifact(t *testing.T) {
	client, backend := fakeClient(t)
	r := &ImageResource{client: client}
	state := createTestImage(t, r, true)
	var old ImageModel
	if d := state.Get(t.Context(), &old); d.HasError() {
		t.Fatal(d)
	}
	plan := tfsdk.Plan(state)
	if d := plan.SetAttribute(t.Context(), path.Root("blueprint"), privateBlueprint(t, types.StringValue("new private contents"))); d.HasError() {
		t.Fatal(d)
	}
	if d := plan.SetAttribute(t.Context(), path.Root("timeouts"), imageTimeoutValues(map[string]string{"update": "1m"})); d.HasError() {
		t.Fatal(d)
	}
	backend.provisionError = context.DeadlineExceeded
	response := resource.UpdateResponse{State: state}
	r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
	if !response.Diagnostics.HasError() || !response.State.Raw.Equal(state.Raw) {
		t.Fatalf("failed reprovision changed state: %v", response.Diagnostics)
	}
	checksum, err := guix.Checksum(t.Context(), old.ArtifactPath.ValueString())
	if err != nil || checksum != old.SHA256.ValueString() {
		t.Fatal("failed reprovision modified the previous private image")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(old.ArtifactPath.ValueString()), "*", "*"))
	if err != nil || len(files) != 0 {
		t.Fatal("failed reprovision retained partial artifacts or GC roots")
	}
}

func TestAccImageOperationTimeouts(t *testing.T) {
	client, backend := fakeClient(t)
	config := func(source, timeout string) string {
		return fmt.Sprintf(`
provider "guix" { artifact_directory = %q }
resource "guix_image" "test" {
  blueprint = { os = %q, channels = "pinned" }
  timeouts = { create = %q, update = "1m", read = "1m" }
}
`, client.ArtifactDirectory, source, timeout)
	}
	var identity, artifact string
	check := func(builds int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			image := state.RootModule().Resources["guix_image.test"].Primary.Attributes
			if identity == "" {
				identity = image["id"]
			}
			if image["id"] != identity || backend.builds != builds {
				return fmt.Errorf("identity changed or unexpected builds: %s, %d", image["id"], backend.builds)
			}
			if artifact != "" && artifact != image["artifact_path"] {
				if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("previous artifact was not released: %s", artifact)
				}
			}
			artifact = image["artifact_path"]
			return nil
		}
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config("v1", "1m"), Check: check(1)},
		{Config: config("v1", "2m"), Check: check(1)},
		{Config: config("v2", "2m"), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("guix_image.test", plancheck.ResourceActionUpdate)}}, Check: check(2)},
		{Config: config("v3", "2m"), Check: check(3)},
		{Config: config("v3", "2m"), PlanOnly: true, ExpectNonEmptyPlan: false},
	}})
}

func TestImageTimeoutStateUpgrade(t *testing.T) {
	r := &ImageResource{}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	for _, timeout := range []string{`"4h"`, `null`} {
		t.Run(timeout, func(t *testing.T) {
			legacy := map[string]json.RawMessage{}
			for name := range schema.Schema.Attributes {
				legacy[name] = json.RawMessage("null")
			}
			legacy["id"] = json.RawMessage(`"11111111111111111111111111111111"`)
			legacy["blueprint"] = json.RawMessage(`{"id":"blueprint-id","os":"blueprint OS","image":null,"machine":null,"channels":"blueprint channels","source_files":{"asset":"public asset"},"secrets":null}`)
			delete(legacy, "timeouts")
			legacy["build_timeout"] = json.RawMessage(timeout)
			contents, _ := json.Marshal(legacy)
			response := resource.UpgradeStateResponse{State: tfsdk.State{Schema: schema.Schema}}
			r.UpgradeState(t.Context())[0].StateUpgrader(t.Context(), resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: contents}}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			var upgraded ImageModel
			if d := response.State.Get(t.Context(), &upgraded); d.HasError() {
				t.Fatal(d)
			}
			want := 4 * time.Hour
			if timeout == "null" {
				want = 2 * time.Hour
			}
			for _, operation := range []string{"create", "update", "read"} {
				duration, diagnostics := imageTimeout(t.Context(), upgraded.Timeouts, operation)
				if diagnostics.HasError() || (operation != "read" && duration != want) || (operation == "read" && duration != 30*time.Second) {
					t.Fatalf("state upgrade lost timeout %s: %v %v", operation, duration, diagnostics)
				}
			}
			if upgraded.ID.ValueString() != "11111111111111111111111111111111" || !upgraded.Blueprint.Equal(testBlueprint(t)) {
				t.Fatal("state upgrade lost image identity or blueprint")
			}
		})
	}
}
