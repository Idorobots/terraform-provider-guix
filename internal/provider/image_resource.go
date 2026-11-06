// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithConfigure = &ImageResource{}
var _ resource.ResourceWithModifyPlan = &ImageResource{}

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
	BuildTimeout types.String `tfsdk:"build_timeout"`
}

func NewImageResource() resource.Resource { return &ImageResource{} }
func (*ImageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_image"
}
func (*ImageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{"blueprint": blueprintSchema(), "system": systemSchema(true)}
	attrs["name"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("image"), MarkdownDescription: "Operator-visible artifact label. Images appear under artifact_directory/<name>/<id>/image while apply waits for manual boot.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Validators: []validator.String{stringCheck("artifact label (letters, digits, underscores and hyphens)")}}
	attrs["id"] = computedString("Unique resource identity, owning an independent image GC root.", true)
	attrs["image_type"] = schema.StringAttribute{Optional: true, MarkdownDescription: "Guix image type for an operating-system configuration, including types exported by pinned channels. Omit to use the pinned Guix default. An image specification determines its own layout, following native Guix behavior.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Validators: []validator.String{stringCheck("nonempty string")}}
	attrs["target"] = schema.StringAttribute{Optional: true, MarkdownDescription: "GNU target triplet for cross-compiling the image, for example aarch64-linux-gnu. Builds use the runner's native system; host system builds and deployments still use system.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Validators: []validator.String{stringCheck("nonempty string")}}
	attrs["artifact_path"] = computedString("Flashable image under artifact_directory/<name>/<id>/image. A private mode-0600 copy when secrets are attached; otherwise a symlink to the clean image.", false)
	attrs["store_path"] = computedString("Clean immutable Guix store image path, retained by the adjacent base-image GC root. Contains no provisioned secrets.", false)
	attrs["sha256"] = computedString("SHA256 checksum of the image bytes.", false)
	attrs["input_digest"] = computedString("Digest of the configuration, channels, system, cross target and source files.", false)
	attrs["build_timeout"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("2h"), MarkdownDescription: "Image build deadline.", Validators: []validator.String{stringCheck("positive duration")}}
	resp.Schema = schema.Schema{MarkdownDescription: "Build a Guix System image with pinned channels, then provision attached private files outside the store using guix secrets image. FUSE mounts are preferred; Polkit authentication is requested when FUSE is unavailable. Flash and boot the final artifact manually. Destroy deletes the owned private copy and releases its clean-image GC root.", Attributes: attrs}
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
	s, known, diagnostics := blueprintSource(ctx, m.Blueprint, m.System, "image")
	resp.Diagnostics.Append(diagnostics...)
	secrets, secretsKnown, secretDiagnostics := blueprintSecrets(ctx, m.Blueprint)
	resp.Diagnostics.Append(secretDiagnostics...)
	if (!known || !secretsKnown) && !resp.Diagnostics.HasError() {
		resp.Diagnostics.AddError("Unknown Guix image inputs", "The selected blueprint specification, channels, source files, system and attached secrets inputs must be known before building.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	duration, err := time.ParseDuration(m.BuildTimeout.ValueString())
	if err != nil || duration <= 0 {
		resp.Diagnostics.AddError("Invalid build timeout", "Expected a positive duration.")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if hasSecrets(m.Blueprint) {
		if err := r.client.Guix.CheckSecrets(ctx, s); err != nil {
			resp.Diagnostics.AddError("Guix secrets extension unavailable", err.Error())
			return
		}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		resp.Diagnostics.AddError("Generate image identity", err.Error())
		return
	}
	m.ID = types.StringValue(hex.EncodeToString(id))
	root := filepath.Join(r.client.ArtifactDirectory, m.Name.ValueString(), m.ID.ValueString(), "image")
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
		resp.Diagnostics.AddError("Guix image build failed", err.Error())
		return
	}
	if hasSecrets(m.Blueprint) {
		checksum, err = r.client.Guix.ProvisionImage(ctx, s, secrets, m.ImageType.ValueString(), m.Target.ValueString(), store, root)
	} else {
		err = os.Symlink(store, root)
	}
	if err != nil {
		resp.Diagnostics.AddError("Guix image provisioning failed", err.Error())
		return
	}
	m.ArtifactPath, m.StorePath, m.SHA256, m.InputDigest = types.StringValue(root), types.StringValue(store), types.StringValue(checksum), types.StringValue(s.ImageDigest(m.Target.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	complete = !resp.Diagnostics.HasError()
}
func (*ImageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	info, err := os.Lstat(m.ArtifactPath.ValueString())
	if errors.Is(err, os.ErrNotExist) {
		if err := releaseImage(m); err != nil {
			resp.Diagnostics.AddError("Release missing image references", err.Error())
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read image artifact", err.Error())
		return
	}
	base, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(m.ArtifactPath.ValueString()), "base-image"))
	if errors.Is(err, os.ErrNotExist) {
		if err := releaseImage(m); err != nil {
			resp.Diagnostics.AddError("Release missing image references", err.Error())
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil || base != m.StorePath.ValueString() {
		resp.Diagnostics.AddError("Image reference changed", "The base-image GC root no longer refers to the recorded clean image.")
		return
	}
	if !hasSecrets(m.Blueprint) {
		resolved, err := filepath.EvalSymlinks(m.ArtifactPath.ValueString())
		if err != nil || resolved != m.StorePath.ValueString() || info.Mode()&os.ModeSymlink == 0 {
			resp.Diagnostics.AddError("Invalid image artifact", "Expected a symlink to the clean image.")
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		resp.Diagnostics.AddError("Invalid private image artifact", "Expected a private regular image file with mode 0600.")
	} else if checksum, err := guix.Checksum(ctx, m.ArtifactPath.ValueString()); err != nil || checksum != m.SHA256.ValueString() {
		resp.Diagnostics.AddError("Private image changed", "The provisioned image no longer matches its recorded checksum.")
	}
}
func (*ImageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Effective build input changes replace the resource. Other blueprint changes
	// and timeout edits only update the descriptor recorded in state.
	var plan ImageModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
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
	s, known, diagnostics := blueprintSource(ctx, plan.Blueprint, plan.System, "image")
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() || req.State.Raw.IsNull() {
		return
	}
	var old ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	previous, previousKnown, previousDiagnostics := blueprintSource(ctx, old.Blueprint, old.System, "image")
	if !known || !previousKnown || previousDiagnostics.HasError() || s.Digest() != previous.Digest() || !secretsEqual(plan.Blueprint, old.Blueprint) {
		resp.RequiresReplace.Append(path.Root("blueprint"))
		return
	}
	if !plan.Name.Equal(old.Name) || !plan.ImageType.Equal(old.ImageType) || !plan.Target.Equal(old.Target) {
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
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(m.ID.ValueString()) || filepath.Base(root) != "image" || filepath.Base(filepath.Dir(root)) != m.ID.ValueString() {
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
