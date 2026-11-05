// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type SecretsResource struct{}

type SecretsModel struct {
	ID    types.String `tfsdk:"id"`
	Files types.Map    `tfsdk:"files"`
}

type secretPaths struct{}

func (secretPaths) Description(context.Context) string {
	return "normalized absolute file paths outside /gnu/store"
}
func (v secretPaths) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (secretPaths) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for name := range req.ConfigValue.Elements() {
		if !path.IsAbs(name) || path.Clean(name) != name || name == "/" || name == "/gnu/store" || strings.HasPrefix(name, "/gnu/store/") {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid secret destination", "File destinations must be normalized absolute paths outside /gnu/store.")
		}
	}
}

func secretFilesSchema() schema.MapNestedAttribute {
	return schema.MapNestedAttribute{
		Required:            true,
		MarkdownDescription: "Files keyed by absolute destination path. Contents are sensitive and remain outside the Guix source bundle. Files are currently descriptors only; no installation is performed. Future installation defaults to root ownership.",
		Validators:          []validator.Map{secretPaths{}},
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"content": schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "File contents, retained as sensitive Terraform state."},
			"mode":    schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("0600"), MarkdownDescription: "Unix file mode for future installation; defaults to 0600.", Validators: []validator.String{stringCheck("octal file mode")}},
		}},
	}
}

func imageSecretsSchema() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:            true,
		MarkdownDescription: "Secret file descriptor, normally guix_secrets.<name>. Records dependencies and sensitive file contents only. This version does not modify the image or install files.",
		Attributes: map[string]schema.Attribute{
			"id":    schema.StringAttribute{Required: true, MarkdownDescription: "Secret descriptor resource identity."},
			"files": secretFilesSchema(),
		},
	}
}

func NewSecretsResource() resource.Resource { return &SecretsResource{} }
func (*SecretsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secrets"
}
func (*SecretsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A logical collection of private files for image or host provisioning. Pass this resource to guix_image.secrets to record dependencies. This version only records descriptors: it neither installs files nor contacts hosts, and has no dependency on SOPS or age. Terraform state contains the file contents.",
		Attributes: map[string]schema.Attribute{
			"id":    computedString("Stable identity of the secret file collection.", true),
			"files": secretFilesSchema(),
		},
	}
}
func (*SecretsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model SecretsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		resp.Diagnostics.AddError("Generate secret descriptor identity", err.Error())
		return
	}
	model.ID = types.StringValue(hex.EncodeToString(id))
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (*SecretsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var model SecretsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	}
}
func (*SecretsResource) Read(context.Context, resource.ReadRequest, *resource.ReadResponse)       {}
func (*SecretsResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
