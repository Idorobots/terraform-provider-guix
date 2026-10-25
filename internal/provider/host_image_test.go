// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

func hostImageType(t *testing.T) types.ObjectType {
	t.Helper()
	imageType, ok := hostImageSchema().GetType().(types.ObjectType)
	if !ok {
		t.Fatal("image schema is not an object")
	}
	return imageType
}

func testHostImage(t *testing.T) types.Object {
	t.Helper()
	image := HostImageModel{
		ID: types.StringValue("image-id"), ArtifactPath: types.StringValue("/tmp/artifacts/server/image"),
		Configuration: types.StringValue("image configuration"), Channels: types.StringValue("image channels"),
		System:      types.StringValue("aarch64-linux"),
		SourceFiles: types.MapValueMust(types.StringType, map[string]attr.Value{"asset": types.StringValue("image asset")}),
	}
	value, diagnostics := types.ObjectValueFrom(t.Context(), hostImageType(t).AttrTypes, image)
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return value
}

func TestReadHostImage(t *testing.T) {
	for _, tc := range []struct {
		name             string
		image            types.Object
		unknown, missing bool
	}{
		{"built image", testHostImage(t), false, false},
		{"pending image", types.ObjectUnknown(hostImageType(t).AttrTypes), true, false},
		{"missing image", types.ObjectNull(hostImageType(t).AttrTypes), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, diagnostics := readHostImage(t.Context(), tc.image)
			if diagnostics.HasError() != tc.missing {
				t.Fatalf("unexpected diagnostics: %v", diagnostics)
			}
			if tc.missing {
				return
			}
			for _, value := range []attr.Value{image.ID, image.ArtifactPath, image.Configuration, image.Channels, image.System, image.SourceFiles} {
				if value.IsUnknown() != tc.unknown {
					t.Fatalf("image value lost its known/unknown state: %s", value)
				}
			}
			if !tc.unknown && image.System.ValueString() != "aarch64-linux" {
				t.Fatalf("image architecture changed: %s", image.System)
			}
		})
	}
}

func TestHostImageCommissioningInfoLog(t *testing.T) {
	client, backend := fakeClient(t)
	r := &HostResource{client: client}
	state := hostState(t, r)
	var model HostModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	model.Image = testHostImage(t)
	sshType, ok := sshSchema().GetType().(types.ObjectType)
	if !ok {
		t.Fatal("SSH schema is not an object")
	}
	client.SSH = types.ObjectValueMust(sshType.AttrTypes, map[string]attr.Value{
		"host_key": types.StringValue("fake host key"), "private_key": types.StringValue("fake management key"),
		"identity_file": types.StringNull(), "user": types.StringNull(), "port": types.Int64Null(),
	})
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	if diagnostics := r.reconcile(ctx, &model, "create"); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	wantSource := guix.Source{Configuration: "image configuration", Channels: "image channels", System: "aarch64-linux", Files: map[string]string{"asset": "image asset"}}
	if backend.observed != desired(wantSource) {
		t.Fatalf("deployed %s; want %s", backend.observed, desired(wantSource))
	}
	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry["@message"] == "Waiting for manually commissioned Guix host" {
			if entry["@level"] != "info" || entry["image_path"] != "/tmp/artifacts/server/image" || entry["image_id"] != "image-id" || entry["instructions"] != commissioningImageInstruction(types.StringValue("/tmp/artifacts/server/image")) {
				t.Fatalf("missing image or manual commissioning INFO details: %v", entry)
			}
			return
		}
	}
	t.Fatal("missing commissioning INFO log")
}
