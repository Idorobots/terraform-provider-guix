// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

var _ resource.ResourceWithConfigure = &ImageResource{}
var _ resource.ResourceWithModifyPlan = &ImageResource{}
var _ resource.ResourceWithUpgradeState = &ImageResource{}

type ImageResource struct{ client *Client }
type ImageModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Blueprint    types.Object `tfsdk:"blueprint"`
	System       types.String `tfsdk:"system"`
	Target       types.String `tfsdk:"target"`
	ImageType    types.String `tfsdk:"image_type"`
	ArtifactPath types.String `tfsdk:"artifact_path"`
	StorePath    types.String `tfsdk:"store_path"`
	SHA256       types.String `tfsdk:"sha256"`
	InputDigest  types.String `tfsdk:"input_digest"`
	Timeouts     types.Object `tfsdk:"timeouts"`
}

func NewImageResource() resource.Resource { return &ImageResource{} }
func (*ImageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_image"
}
func (*ImageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{"blueprint": blueprintSchema(), "system": systemSchema(false)}
	attrs["name"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("image"), MarkdownDescription: "Operator-visible artifact label. Images appear under artifact_directory/<name>/<id>/, with separate generations for rebuilds.", Validators: []validator.String{stringCheck("artifact label (letters, digits, underscores and hyphens)")}}
	attrs["id"] = computedString("Unique resource identity, owning an independent image GC root.", true)
	attrs["image_type"] = schema.StringAttribute{Optional: true, MarkdownDescription: "Guix image type for an operating-system configuration, including types exported by pinned channels. Omit to use the pinned Guix default. An image specification determines its own layout, following native Guix behavior.", Validators: []validator.String{stringCheck("nonempty string")}}
	attrs["target"] = schema.StringAttribute{Optional: true, MarkdownDescription: "GNU target triplet for cross-compiling the image, for example aarch64-linux-gnu. Builds use the runner's native system; host system builds and deployments still use system.", Validators: []validator.String{stringCheck("nonempty string")}}
	attrs["artifact_path"] = computedString("Flashable image under artifact_directory/<name>/<id>/, with a new generation on rebuild. A private mode-0600 copy after secret provisioning; otherwise a symlink to the clean image. A provisioning warning means the declared secrets must be installed manually before booting.", false)
	attrs["store_path"] = computedString("Clean immutable Guix store image path, retained by the adjacent base-image GC root. Contains no provisioned secrets.", false)
	attrs["sha256"] = computedString("SHA256 checksum of the image bytes.", false)
	attrs["input_digest"] = computedString("Digest of the configuration, channels, system, cross target and source files.", false)
	attrs["timeouts"] = schema.SingleNestedAttribute{Optional: true, MarkdownDescription: "Operation deadlines. Defaults: create 2h, update 2h, read 30s.", Attributes: map[string]schema.Attribute{
		"create": schema.StringAttribute{Optional: true, MarkdownDescription: "Initial image build and provisioning timeout.", Validators: []validator.String{stringCheck("positive duration")}},
		"update": schema.StringAttribute{Optional: true, MarkdownDescription: "Image rebuild and provisioning timeout. The previous artifact is retained until the new image is ready.", Validators: []validator.String{stringCheck("positive duration")}},
		"read":   schema.StringAttribute{Optional: true, MarkdownDescription: "Artifact existence and reference check timeout. Refresh does not build or checksum the image.", Validators: []validator.String{stringCheck("positive duration")}},
	}}
	resp.Schema = schema.Schema{Version: 1, MarkdownDescription: "Build a Guix System image with pinned channels, then provision attached private files outside the store using guix secrets image. FUSE mounts are preferred; Polkit authentication is requested when FUSE is unavailable. If provisioning fails, retain the clean image and warn that the declared secrets must be installed manually before booting. Input changes rebuild the image under the update deadline, retaining the previous artifact until success. Flash and boot the final artifact manually. Destroy deletes the owned private copy and releases its clean-image GC root.", Attributes: attrs}
}

func (*ImageResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{0: {StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
		var state map[string]json.RawMessage
		if err := json.Unmarshal(req.RawState.JSON, &state); err != nil {
			resp.Diagnostics.AddError("Upgrade image state", err.Error())
			return
		}
		timeout := state["build_timeout"]
		if len(timeout) == 0 || string(timeout) == "null" {
			timeout = json.RawMessage(`"2h"`)
		}
		state["timeouts"], _ = json.Marshal(map[string]json.RawMessage{"create": timeout, "update": timeout, "read": json.RawMessage("null")})
		delete(state, "build_timeout")
		contents, err := json.Marshal(state)
		if err == nil {
			raw := tfprotov6.RawState{JSON: contents}
			resp.State.Raw, err = raw.Unmarshal(resp.State.Schema.Type().TerraformType(ctx))
		}
		if err != nil {
			resp.Diagnostics.AddError("Upgrade image state", err.Error())
		}
	}}}
}

func imageTimeout(ctx context.Context, object types.Object, operation string) (time.Duration, diag.Diagnostics) {
	duration := map[string]time.Duration{"create": 2 * time.Hour, "update": 2 * time.Hour, "read": 30 * time.Second}[operation]
	var diagnostics diag.Diagnostics
	if object.IsNull() {
		return duration, diagnostics
	}
	var model TimeoutsModel
	diagnostics.Append(object.As(ctx, &model, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() {
		return duration, diagnostics
	}
	value := map[string]types.String{"create": model.Create, "update": model.Update, "read": model.Read}[operation]
	if !value.IsNull() {
		parsed, err := time.ParseDuration(value.ValueString())
		if err != nil || parsed <= 0 {
			diagnostics.AddError("Invalid image timeout", "Expected a positive duration for "+operation+".")
		} else {
			duration = parsed
		}
	}
	return duration, diagnostics
}
func (r *ImageResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	var err error
	r.client, err = configureClient(req.ProviderData)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
	}
}

func (r *ImageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.build(ctx, &m, "create")...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		_ = releaseImage(m)
	}
}

func (r *ImageResource) build(ctx context.Context, m *ImageModel, operation string) diag.Diagnostics {
	var result diag.Diagnostics
	s, known, diagnostics := blueprintSource(ctx, m.Blueprint, m.System, "image")
	result.Append(diagnostics...)
	secrets, secretsKnown, secretDiagnostics := blueprintSecrets(ctx, m.Blueprint)
	result.Append(secretDiagnostics...)
	if (!known || !secretsKnown) && !result.HasError() {
		result.AddError("Unknown Guix image inputs", "The selected blueprint specification, channels, source files, system and attached secrets inputs must be known before building.")
	}
	duration, timeoutDiagnostics := imageTimeout(ctx, m.Timeouts, operation)
	result.Append(timeoutDiagnostics...)
	if result.HasError() {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if hasSecrets(m.Blueprint) {
		if err := r.client.Guix.CheckSecrets(ctx, s); err != nil {
			result.AddError("Guix secrets extension unavailable", err.Error())
			return result
		}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		result.AddError("Generate image identity", err.Error())
		return result
	}
	generation := hex.EncodeToString(id)
	if operation == "create" {
		m.ID = types.StringValue(generation)
	}
	directory := filepath.Join(r.client.ArtifactDirectory, m.Name.ValueString(), m.ID.ValueString())
	if operation == "update" {
		directory = filepath.Join(directory, generation)
	}
	root := filepath.Join(directory, "image")
	base := filepath.Join(filepath.Dir(root), "base-image")
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(root)
			_ = os.Remove(base)
			_ = os.Remove(filepath.Dir(root))
		}
	}()
	store, checksum, err := r.client.Guix.BuildImage(ctx, s, m.ImageType.ValueString(), m.Target.ValueString(), base)
	if err != nil {
		result.AddError("Guix image build failed", err.Error())
		return result
	}
	if hasSecrets(m.Blueprint) {
		var provisionedChecksum string
		provisionedChecksum, err = r.client.Guix.ProvisionImage(ctx, s, secrets, m.ImageType.ValueString(), m.Target.ValueString(), store, root)
		if err == nil {
			checksum = provisionedChecksum
		} else if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			result.AddWarning("Image secrets not provisioned", fmt.Sprintf("Secrets were not provisioned in the image. You must manually install the files declared in blueprint.secrets at their configured destinations before booting.\n\n%s", err))
			// Discard any failed output and retain the clean image.
			if err = os.Remove(root); err == nil || errors.Is(err, os.ErrNotExist) {
				err = os.Symlink(store, root)
			}
		}
	} else {
		err = os.Symlink(store, root)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		result.AddError("Guix image provisioning failed", err.Error())
		return result
	}
	m.ArtifactPath, m.StorePath, m.SHA256, m.InputDigest = types.StringValue(root), types.StringValue(store), types.StringValue(checksum), types.StringValue(s.ImageDigest(m.Target.ValueString()))
	complete = true
	return result
}
func (*ImageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	duration, diagnostics := imageTimeout(ctx, m.Timeouts, "read")
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if err := ctx.Err(); err != nil {
		resp.Diagnostics.AddError("Read image artifact", err.Error())
		return
	}
	type result struct {
		missing bool
		err     error
	}
	// Filesystem operations can block on a remote mount. Only the read-only check
	// runs asynchronously: a late result cannot remove files or change state.
	checked := make(chan result, 1)
	go func() {
		missing, err := checkImage(m)
		checked <- result{missing, err}
	}()
	select {
	case <-ctx.Done():
		resp.Diagnostics.AddError("Read image artifact", ctx.Err().Error())
	case check := <-checked:
		if err := ctx.Err(); err != nil {
			resp.Diagnostics.AddError("Read image artifact", err.Error())
		} else if check.err != nil {
			resp.Diagnostics.AddError("Read image artifact", check.err.Error())
		} else if check.missing {
			if err := releaseImage(m); err != nil {
				resp.Diagnostics.AddError("Release missing image references", err.Error())
				return
			}
			resp.State.RemoveResource(ctx)
		}
	}
}

func checkImage(m ImageModel) (bool, error) {
	info, err := os.Lstat(m.ArtifactPath.ValueString())
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	base, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(m.ArtifactPath.ValueString()), "base-image"))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || base != m.StorePath.ValueString() {
		return false, fmt.Errorf("the base-image GC root no longer refers to the recorded clean image")
	}
	if !hasSecrets(m.Blueprint) || info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(m.ArtifactPath.ValueString())
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil || resolved != m.StorePath.ValueString() || info.Mode()&os.ModeSymlink == 0 {
			return false, fmt.Errorf("expected a symlink to the clean image")
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return false, fmt.Errorf("expected a private regular image file with mode 0600")
	}
	return false, nil
}
func (r *ImageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, old ImageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if imageInputsChanged(ctx, plan, old) {
		resp.Diagnostics.Append(r.build(ctx, &plan, "update")...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if plan.ArtifactPath.Equal(old.ArtifactPath) {
		return
	}
	if resp.Diagnostics.HasError() {
		_ = releaseImage(plan)
		return
	}
	if err := releaseImage(old); err != nil {
		resp.Diagnostics.AddWarning("Release previous image", err.Error())
	}
}

func imageInputsChanged(ctx context.Context, plan, old ImageModel) bool {
	s, known, diagnostics := blueprintSource(ctx, plan.Blueprint, plan.System, "image")
	previous, previousKnown, previousDiagnostics := blueprintSource(ctx, old.Blueprint, old.System, "image")
	return !known || !previousKnown || diagnostics.HasError() || previousDiagnostics.HasError() || s.Digest() != previous.Digest() || !secretsEqual(plan.Blueprint, old.Blueprint) || !plan.Name.Equal(old.Name) || !plan.ImageType.Equal(old.ImageType) || !plan.Target.Equal(old.Target)
}
func (*ImageResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan ImageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, _, diagnostics := blueprintSource(ctx, plan.Blueprint, plan.System, "image")
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() || req.State.Raw.IsNull() {
		return
	}
	var old ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if imageInputsChanged(ctx, plan, old) {
		return
	}
	for name, value := range map[string]types.String{"artifact_path": old.ArtifactPath, "store_path": old.StorePath, "sha256": old.SHA256, "input_digest": old.InputDigest} {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), value)...)
	}
}
func (*ImageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := releaseImage(m); err != nil {
		resp.Diagnostics.AddError("Release image files", err.Error())
	}
}

func releaseImage(m ImageModel) error {
	root := m.ArtifactPath.ValueString()
	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	directory := filepath.Dir(root)
	owned := filepath.Base(directory) == m.ID.ValueString() || (filepath.Base(filepath.Dir(directory)) == m.ID.ValueString() && idPattern.MatchString(filepath.Base(directory)))
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !idPattern.MatchString(m.ID.ValueString()) || filepath.Base(root) != "image" || !owned {
		return fmt.Errorf("refusing to release unrecognized image reference %q", root)
	}
	base := filepath.Join(filepath.Dir(root), "base-image")
	for _, file := range []string{base, root} {
		info, err := os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if file == base {
			link, err := os.Readlink(file)
			if err != nil || link != m.StorePath.ValueString() {
				return fmt.Errorf("unrecognized base-image GC root")
			}
		} else if info.Mode()&os.ModeSymlink == 0 && (!info.Mode().IsRegular() || !hasSecrets(m.Blueprint)) {
			return fmt.Errorf("expected an owned image file or symlink")
		}
	}
	for _, file := range []string{root, base} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Remove only the now-empty owned directory; never recursively remove artifacts.
	_ = os.Remove(filepath.Dir(root))
	return nil
}
