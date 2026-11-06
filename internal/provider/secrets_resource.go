// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path"
	"strings"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
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
		if !path.IsAbs(name) || path.Clean(name) != name || strings.ContainsRune(name, 0) || name == "/" || name == "/gnu/store" || strings.HasPrefix(name, "/gnu/store/") {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid secret destination", "File destinations must be normalized absolute paths outside /gnu/store.")
		}
		for other := range req.ConfigValue.Elements() {
			if strings.HasPrefix(other, name+"/") {
				resp.Diagnostics.AddAttributeError(req.Path, "Overlapping secret destinations", "A secret file destination cannot be a parent directory of another secret file.")
			}
		}
	}
}

func secretFilesSchema() schema.MapNestedAttribute {
	return schema.MapNestedAttribute{
		Required:            true,
		MarkdownDescription: "Files keyed by absolute destination path. Consuming images and hosts install these files outside /gnu/store with root ownership. Contents are sensitive and remain outside the Guix source bundle.",
		Validators:          []validator.Map{secretPaths{}},
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"content": schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "File contents, retained as sensitive Terraform state."},
			"mode":    schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("0600"), MarkdownDescription: "Installed Unix file mode; defaults to 0600.", Validators: []validator.String{stringCheck("octal file mode")}},
		}},
	}
}

func blueprintSecretsSchema() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional:            true,
		MarkdownDescription: "Private files, normally guix_secrets.<name>. Images install these after building; hosts install them before any required Guix deployment.",
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
		MarkdownDescription: "A logical collection of private files. Attach it to guix_blueprint.secrets for installation by image and host consumers through the bundled guix secrets extension. This descriptor itself performs no I/O and has no SOPS or age dependency. Terraform state contains the file contents. Removing files or destroying this descriptor does not delete files from running hosts.",
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

func blueprintSecretFiles(ctx context.Context, value types.Object) (guix.SecretFiles, bool, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	files := guix.SecretFiles{}
	if value.IsUnknown() {
		return files, false, diagnostics
	}
	if value.IsNull() {
		return files, true, diagnostics
	}
	var blueprint BlueprintModel
	diagnostics.Append(value.As(ctx, &blueprint, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() {
		return files, false, diagnostics
	}
	if blueprint.Secrets.IsUnknown() {
		return files, false, diagnostics
	}
	if blueprint.Secrets.IsNull() {
		return files, true, diagnostics
	}
	var secrets SecretsModel
	diagnostics.Append(blueprint.Secrets.As(ctx, &secrets, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() || secrets.Files.IsUnknown() {
		return files, false, diagnostics
	}
	for name, value := range secrets.Files.Elements() {
		if value.IsUnknown() {
			return files, false, diagnostics
		}
		var file struct {
			Content types.String `tfsdk:"content"`
			Mode    types.String `tfsdk:"mode"`
		}
		object, ok := value.(types.Object)
		if !ok || object.IsNull() {
			diagnostics.AddError("Invalid secret file", "Every secret file must contain content and a file mode.")
			return files, false, diagnostics
		}
		diagnostics.Append(object.As(ctx, &file, basetypes.ObjectAsOptions{})...)
		if file.Content.IsUnknown() || file.Mode.IsUnknown() {
			return files, false, diagnostics
		}
		mode := file.Mode.ValueString()
		if mode == "" {
			mode = "0600"
		}
		files[name] = guix.SecretFile{Content: file.Content.ValueString(), Mode: mode}
	}
	return files, !diagnostics.HasError(), diagnostics
}
