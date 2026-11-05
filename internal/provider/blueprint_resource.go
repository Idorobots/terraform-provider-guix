// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type BlueprintResource struct{}

type BlueprintModel struct {
	ID          types.String `tfsdk:"id"`
	OS          types.String `tfsdk:"os"`
	Image       types.String `tfsdk:"image"`
	Machine     types.String `tfsdk:"machine"`
	Channels    types.String `tfsdk:"channels"`
	SourceFiles types.Map    `tfsdk:"source_files"`
	Secrets     types.Object `tfsdk:"secrets"`
}

func blueprintAttributes() map[string]schema.Attribute {
	attrs := map[string]schema.Attribute{
		"channels":     schema.StringAttribute{Required: true, MarkdownDescription: "Pinned channels Scheme source, normally produced by guix describe -f channels. Used with guix time-machine.", Validators: []validator.String{stringCheck("nonempty string")}},
		"source_files": schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Public supporting files keyed by normalized relative path, including Scheme modules, local assets and encrypted documents. Provider filenames are reserved.", Validators: []validator.Map{sourcePaths{}}},
		"secrets":      blueprintSecretsSchema(),
	}
	for name, result := range map[string]string{"os": "operating-system", "image": "image", "machine": "machine"} {
		attrs[name] = schema.StringAttribute{Optional: true, MarkdownDescription: "Scheme source evaluating to a Guix " + result + " record. Supply raw source, file(), templatefile(), or an explicit (load \"...\"); filenames are not loaded automatically. Evaluated only by a consuming leaf that selects this specification.", Validators: []validator.String{stringCheck("nonempty string")}}
	}
	return attrs
}

func blueprintSchema() schema.SingleNestedAttribute {
	attrs := blueprintAttributes()
	attrs["id"] = schema.StringAttribute{Optional: true, MarkdownDescription: "Blueprint resource identity. Normally supplied by guix_blueprint.<name>; not a build input."}
	return schema.SingleNestedAttribute{Required: true, MarkdownDescription: "Shared configuration descriptor, normally guix_blueprint.<name>. Contains optional OS, image and machine specifications, pinned channels, supporting sources and secret descriptors. Architecture and operation settings belong to this leaf.", Attributes: attrs}
}

type sourcePaths struct{}

func (sourcePaths) Description(context.Context) string {
	return "normalized, non-reserved relative source paths"
}
func (v sourcePaths) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (sourcePaths) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for name := range req.ConfigValue.Elements() {
		if !guix.ValidSourcePath(name) {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid source file", fmt.Sprintf("Invalid or reserved relative path %q.", name))
		}
	}
}

// blueprintSource selects only this operation's effective public inputs. Unknown
// preferred specifications defer selection; they must not fall back to the OS.
// Secret descriptors and unused specifications never enter a Guix source digest.
func blueprintSource(ctx context.Context, value types.Object, system types.String, preferred string) (guix.Source, bool, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	var s guix.Source
	if value.IsUnknown() {
		return s, false, diagnostics
	}
	if value.IsNull() {
		diagnostics.AddAttributeError(path.Root("blueprint"), "Missing Guix blueprint", "Set blueprint = guix_blueprint.<name>.")
		return s, false, diagnostics
	}
	var m BlueprintModel
	diagnostics.Append(value.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() {
		return s, false, diagnostics
	}
	selected := map[string]types.String{"image": m.Image, "machine": m.Machine}[preferred]
	s.Kind = preferred
	if selected.IsNull() {
		selected, s.Kind = m.OS, "os"
	}
	if selected.IsNull() {
		diagnostics.AddAttributeError(path.Root("blueprint"), "Missing Guix specification", "This resource requires blueprint."+preferred+" or blueprint.os.")
		return s, false, diagnostics
	}
	if selected.IsUnknown() || m.Channels.IsUnknown() || system.IsUnknown() || m.SourceFiles.IsUnknown() {
		return s, false, diagnostics
	}
	s.Configuration, s.Channels, s.System = selected.ValueString(), m.Channels.ValueString(), system.ValueString()
	for _, contents := range m.SourceFiles.Elements() {
		if contents.IsUnknown() {
			return s, false, diagnostics
		}
	}
	if !m.SourceFiles.IsNull() {
		diagnostics.Append(m.SourceFiles.ElementsAs(ctx, &s.Files, false)...)
	}
	if len(s.Files) == 0 {
		s.Files = nil
	}
	return s, !diagnostics.HasError(), diagnostics
}

func NewBlueprintResource() resource.Resource { return &BlueprintResource{} }
func (*BlueprintResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_blueprint"
}
func (*BlueprintResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := blueprintAttributes()
	attrs["id"] = computedString("Stable identity of the configuration descriptor.", true)
	resp.Schema = schema.Schema{MarkdownDescription: "A reusable Guix configuration blueprint. OS, image and machine specifications are independently optional, even all absent; consuming resources validate what they need. This resource only records configuration: it does not evaluate Scheme, build artifacts, install secrets or contact hosts.", Attributes: attrs}
}
func (*BlueprintResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m BlueprintModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		resp.Diagnostics.AddError("Generate blueprint identity", err.Error())
		return
	}
	m.ID = types.StringValue(hex.EncodeToString(id))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
func (*BlueprintResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m BlueprintModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	}
}
func (*BlueprintResource) Read(context.Context, resource.ReadRequest, *resource.ReadResponse)       {}
func (*BlueprintResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
