// SPDX-License-Identifier: MPL-2.0
package provider

import (
	"context"

	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

func blueprintSecretsSchema() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true, Sensitive: true,
		MarkdownDescription: "Private Scheme source evaluating to a (guix secrets) secrets record. Accepts raw source, file(), templatefile(), or an explicit load expression. Evaluated by image and host consumers during apply, outside public Guix build inputs. Relative sources and load expressions use the Terraform working directory; use absolute paths for module-local files. Requires the installed guix secrets extension in the pinned channels. External file changes can trigger image replacement through terraform_data and lifecycle.replace_triggered_by; see the provider's private file provisioning documentation.",
		Validators:          []validator.String{stringCheck("nonempty string")},
	}
}

func hasSecrets(value types.Object) bool {
	return !value.IsNull() && !value.IsUnknown() && !value.Attributes()["secrets"].IsNull()
}

func secretsEqual(a, b types.Object) bool {
	if a.IsNull() || b.IsNull() || a.IsUnknown() || b.IsUnknown() {
		return a.Equal(b)
	}
	return a.Attributes()["secrets"].Equal(b.Attributes()["secrets"])
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
	result.Expression = blueprint.Secrets.ValueString()
	return result, true, diagnostics
}
