// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

type stringCheck string

func (v stringCheck) Description(context.Context) string             { return string(v) }
func (v stringCheck) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (v stringCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	valid := true
	switch v {
	case "mDNS hostname ending in .local":
		valid = discovery.ValidHostname(s)
	case "positive duration":
		d, err := time.ParseDuration(s)
		valid = err == nil && d > 0
	case "nonempty string":
		valid = strings.TrimSpace(s) != ""
	case "artifact label (letters, digits, underscores and hyphens)":
		valid = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(s)
	case "octal file mode":
		valid = regexp.MustCompile(`^0[0-7]{3}$`).MatchString(s)
	}
	if !valid {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", "Expected "+string(v)+".")
	}
}

func systemSchema(immutable bool) schema.StringAttribute {
	a := schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("x86_64-linux"), MarkdownDescription: "Guix native build system; defaults to x86_64-linux. Foreign-system builds require emulation or offloading. This does not request cross-compilation.", Validators: []validator.String{stringCheck("nonempty string")}}
	if immutable {
		a.PlanModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
	}
	return a
}

func computedString(description string, stable bool) schema.StringAttribute {
	a := schema.StringAttribute{Computed: true, MarkdownDescription: description}
	if stable {
		a.PlanModifiers = []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	}
	return a
}

type SSHModel struct {
	User         types.String `tfsdk:"user"`
	Port         types.Int64  `tfsdk:"port"`
	HostKey      types.String `tfsdk:"host_key"`
	PrivateKey   types.String `tfsdk:"private_key"`
	IdentityFile types.String `tfsdk:"identity_file"`
}

func destination(ctx context.Context, object types.Object, fallback types.Object) (guix.Destination, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	if object.IsNull() {
		object = fallback
	}
	if object.IsNull() || object.IsUnknown() {
		diagnostics.AddError("Missing SSH configuration", "Set ssh on the host or provider, including the pinned host_key and management identity.")
		return guix.Destination{}, diagnostics
	}
	var m SSHModel
	diagnostics.Append(object.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	d := guix.Destination{User: m.User.ValueString(), Port: int(m.Port.ValueInt64()), HostKey: m.HostKey.ValueString(), PrivateKey: m.PrivateKey.ValueString(), IdentityFile: m.IdentityFile.ValueString()}
	if d.User == "" {
		d.User = "root"
	}
	if m.Port.IsNull() {
		d.Port = 22
	}
	if d.Port < 1 || d.Port > 65535 {
		diagnostics.AddError("Invalid SSH port", "SSH port must be between 1 and 65535.")
	}
	if d.HostKey == "" || (d.PrivateKey == "") == (d.IdentityFile == "") {
		diagnostics.AddError("Invalid SSH configuration", "Supply host_key and exactly one of private_key or identity_file.")
	}
	return d, diagnostics
}

func sshSchema() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{Optional: true, MarkdownDescription: "SSH settings overriding the provider defaults. Management private keys authenticate connections; host_key pins the server identity.", Attributes: map[string]schema.Attribute{
		"user":          schema.StringAttribute{Optional: true, MarkdownDescription: "SSH user; defaults to root."},
		"port":          schema.Int64Attribute{Optional: true, MarkdownDescription: "SSH port; defaults to 22."},
		"host_key":      schema.StringAttribute{Required: true, MarkdownDescription: "Pinned OpenSSH host public key."},
		"private_key":   schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Unencrypted management private key. Mutually exclusive with identity_file."},
		"identity_file": schema.StringAttribute{Optional: true, MarkdownDescription: "Runner path to an unencrypted management private key."},
	}}
}
