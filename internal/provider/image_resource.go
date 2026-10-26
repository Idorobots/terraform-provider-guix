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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithConfigure = &ImageResource{}

type ImageResource struct{ client *Client }
type ImageModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	Configuration    types.String `tfsdk:"configuration"`
	Channels         types.String `tfsdk:"channels"`
	System           types.String `tfsdk:"system"`
	SourceFiles      types.Map    `tfsdk:"source_files"`
	ImageType        types.String `tfsdk:"image_type"`
	ImageDefinition  types.String `tfsdk:"image_definition"`
	SystemDefinition types.String `tfsdk:"system_definition"`
	ArtifactPath     types.String `tfsdk:"artifact_path"`
	StorePath        types.String `tfsdk:"store_path"`
	SHA256           types.String `tfsdk:"sha256"`
	InputDigest      types.String `tfsdk:"input_digest"`
	BuildTimeout     types.String `tfsdk:"build_timeout"`
}

func NewImageResource() resource.Resource { return &ImageResource{} }
func (*ImageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_image"
}
func (*ImageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := sourceSchema(true)
	attrs["name"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("image"), MarkdownDescription: "Operator-visible artifact label. Images appear under artifact_directory/<name>/<id>/image while apply waits for manual boot.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Validators: []validator.String{stringCheck("artifact label (letters, digits, underscores and hyphens)")}}
	attrs["id"] = computedString("Unique resource identity, owning an independent image GC root.", true)
	attrs["image_type"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("efi-raw"), MarkdownDescription: "Guix system image type.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Validators: []validator.String{stringCheck("nonempty string")}}
	attrs["image_definition"] = definitionSchema("Scheme source exporting custom image types, loaded before image_type lookup inside pinned Guix. Use file() or templatefile(); supporting modules and assets belong in source_files.", true)
	attrs["system_definition"] = definitionSchema("Scheme source exporting custom platforms referenced by system, loaded before target-system validation inside pinned Guix. Separate from the operating-system configuration. Use file() or templatefile().", true)
	attrs["artifact_path"] = computedString("Persistent image symlink and Guix GC root. Available under artifact_directory/<name>/<id>/image during manual commissioning.", false)
	attrs["store_path"] = computedString("Immutable Guix store image path.", false)
	attrs["sha256"] = computedString("SHA256 checksum of the image bytes.", false)
	attrs["input_digest"] = computedString("Digest of the configuration, channels, target system, custom definitions and source files.", false)
	attrs["build_timeout"] = schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("2h"), MarkdownDescription: "Image build deadline.", Validators: []validator.String{stringCheck("positive duration")}}
	resp.Schema = schema.Schema{MarkdownDescription: "Build an immutable Guix System image with pinned channels. Flash and boot it manually; the provider retains it using a per-resource GC root. Destroy releases only that reference.", Attributes: attrs}
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
	s, diagnostics := source(ctx, m.Configuration, m.Channels, m.System, m.SourceFiles)
	s.ImageDefinition = m.ImageDefinition.ValueString()
	s.SystemDefinition = m.SystemDefinition.ValueString()
	resp.Diagnostics.Append(diagnostics...)
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
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		resp.Diagnostics.AddError("Generate image identity", err.Error())
		return
	}
	m.ID = types.StringValue(hex.EncodeToString(id))
	root := filepath.Join(r.client.ArtifactDirectory, m.Name.ValueString(), m.ID.ValueString(), "image")
	store, checksum, err := r.client.Guix.BuildImage(ctx, s, m.ImageType.ValueString(), root)
	if err != nil {
		_ = os.Remove(root)
		resp.Diagnostics.AddError("Guix image build failed", err.Error())
		return
	}
	m.ArtifactPath, m.StorePath, m.SHA256, m.InputDigest = types.StringValue(root), types.StringValue(store), types.StringValue(checksum), types.StringValue(s.Digest())
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
func (*ImageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resolved, err := filepath.EvalSymlinks(m.ArtifactPath.ValueString())
	if errors.Is(err, os.ErrNotExist) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read image artifact", err.Error())
		return
	}
	if resolved != m.StorePath.ValueString() {
		resp.Diagnostics.AddError("Image reference changed", "The image GC root no longer refers to the recorded store path.")
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		resp.Diagnostics.AddError("Read image artifact", err.Error())
		return
	}
	if !info.Mode().IsRegular() {
		resp.Diagnostics.AddError("Invalid image artifact", "Expected a regular image file.")
	}
}
func (*ImageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Only the build timeout can change without replacement.
	var old, plan ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	old.BuildTimeout = plan.BuildTimeout
	resp.Diagnostics.Append(resp.State.Set(ctx, &old)...)
}
func (*ImageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m ImageModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	root := m.ArtifactPath.ValueString()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(m.ID.ValueString()) || filepath.Base(root) != "image" || filepath.Base(filepath.Dir(root)) != m.ID.ValueString() {
		resp.Diagnostics.AddError("Invalid image ownership", fmt.Sprintf("Refusing to release unrecognized image reference %q.", root))
		return
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Release image reference", err.Error())
		return
	}
	if info.Mode()&os.ModeSymlink == 0 {
		resp.Diagnostics.AddError("Invalid image reference", "Expected a GC root symlink.")
		return
	}
	if err := os.Remove(root); err != nil {
		resp.Diagnostics.AddError("Release image reference", err.Error())
		return
	}
	// Remove only the now-empty owned directory; never recursively remove artifacts.
	_ = os.Remove(filepath.Dir(root))
}
