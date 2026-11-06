// SPDX-License-Identifier: MPL-2.0
package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type SecretsResource struct{}
type SecretsModel struct {
	ID          types.String `tfsdk:"id"`
	Secrets     types.String `tfsdk:"secrets"`
	FileDigests types.Map    `tfsdk:"file_digests"`
}

func secretsAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"secrets":      schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "Scheme source evaluating to a (guix secrets) secrets-configuration record. Accepts raw source, file(), templatefile(), or an explicit load expression. Evaluated only by a consuming leaf during apply. Relative sources and load expressions use the Terraform working directory; use absolute paths when referencing module-local files.", Validators: []validator.String{stringCheck("nonempty string")}},
		"file_digests": schema.MapAttribute{Optional: true, Sensitive: true, ElementType: types.StringType, MarkdownDescription: "Explicit file change triggers, normally { name = filesha256(path) }. Terraform recalculates these during planning. Use for source files read by Scheme; the provider does not discover or evaluate private sources during planning. Keys are descriptive names, values are digests. Excluded from public Guix build inputs."},
	}
}

func blueprintSecretsSchema() schema.SingleNestedAttribute {
	attrs := secretsAttributes()
	attrs["id"] = schema.StringAttribute{Optional: true, MarkdownDescription: "Secret descriptor resource identity."}
	return schema.SingleNestedAttribute{Optional: true, MarkdownDescription: "Private provisioning declaration, normally guix_secrets.<name>. Images install it after building; hosts install it before any required deployment. Requires the installed guix secrets extension in the pinned channels.", Attributes: attrs}
}

func NewSecretsResource() resource.Resource { return &SecretsResource{} }
func (*SecretsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secrets"
}
func (*SecretsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := secretsAttributes()
	attrs["id"] = computedString("Stable identity of the secret declaration.", true)
	resp.Schema = schema.Schema{MarkdownDescription: "A private Scheme secrets declaration. Attach it to guix_blueprint.secrets for image or SSH provisioning through the installed guix secrets extension. This descriptor performs no Scheme evaluation or remote I/O. Inline contents remain in sensitive Terraform state. Removing this descriptor does not delete installed files.", Attributes: attrs}
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

func hasSecrets(value types.Object) bool {
	return !value.IsNull() && !value.IsUnknown() && !value.Attributes()["secrets"].IsNull()
}

func secretsEqual(a, b types.Object) bool {
	if a.IsNull() || b.IsNull() || a.IsUnknown() || b.IsUnknown() {
		return a.Equal(b)
	}
	left, right := a.Attributes()["secrets"], b.Attributes()["secrets"]
	if left.IsNull() || right.IsNull() || left.IsUnknown() || right.IsUnknown() {
		return left.Equal(right)
	}
	// Descriptor identity is metadata, just as blueprint.id is not a build input.
	first, firstOK := left.(types.Object)
	second, secondOK := right.(types.Object)
	if !firstOK || !secondOK {
		return false
	}
	firstAttrs, secondAttrs := first.Attributes(), second.Attributes()
	return firstAttrs["secrets"].Equal(secondAttrs["secrets"]) && firstAttrs["file_digests"].Equal(secondAttrs["file_digests"])
}

func blueprintSecrets(ctx context.Context, value types.Object) (guix.SecretConfiguration, bool, diag.Diagnostics) {
	var result guix.SecretConfiguration
	var diagnostics diag.Diagnostics
	if value.IsUnknown() {
		return result, false, diagnostics
	}
	if !hasSecrets(value) {
		return result, true, diagnostics
	}
	var blueprint BlueprintModel
	diagnostics.Append(value.As(ctx, &blueprint, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() || blueprint.Secrets.IsUnknown() {
		return result, false, diagnostics
	}
	var model SecretsModel
	diagnostics.Append(blueprint.Secrets.As(ctx, &model, basetypes.ObjectAsOptions{})...)
	if diagnostics.HasError() || model.Secrets.IsUnknown() || model.FileDigests.IsUnknown() {
		return result, false, diagnostics
	}
	result.Expression = model.Secrets.ValueString()
	if !model.FileDigests.IsNull() {
		for _, v := range model.FileDigests.Elements() {
			if v.IsUnknown() {
				return result, false, diagnostics
			}
		}
		diagnostics.Append(model.FileDigests.ElementsAs(ctx, &result.FileDigests, false)...)
	}
	return result, !diagnostics.HasError(), diagnostics
}
