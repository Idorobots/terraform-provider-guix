// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
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
	}
	if !valid {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", "Expected "+string(v)+".")
	}
}

func sourceSchema(required bool) map[string]schema.Attribute {
	var stringModifiers []planmodifier.String
	var mapModifiers []planmodifier.Map
	if required {
		// Image sources are immutable; host sources are reconciled in place.
		stringModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
		mapModifiers = []planmodifier.Map{mapplanmodifier.RequiresReplace()}
	}
	return map[string]schema.Attribute{
		"configuration": schema.StringAttribute{Required: required, Optional: !required, MarkdownDescription: "Guix operating-system expression as Scheme source. Use file() or templatefile() so OpenTofu tracks changes.", Validators: []validator.String{stringCheck("nonempty string")}, PlanModifiers: stringModifiers},
		"channels":      schema.StringAttribute{Required: required, Optional: !required, MarkdownDescription: "Pinned channels Scheme source, normally produced by guix describe -f channels. Used with guix time-machine.", Validators: []validator.String{stringCheck("nonempty string")}, PlanModifiers: stringModifiers},
		"system":        schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("x86_64-linux"), MarkdownDescription: "Guix target system.", Validators: []validator.String{stringCheck("nonempty string")}, PlanModifiers: stringModifiers},
		"source_files":  schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Supporting files keyed by relative path, including Scheme modules and local assets. Provider filenames and path traversal are rejected.", PlanModifiers: mapModifiers},
	}
}

func computedString(description string, stable bool) schema.StringAttribute {
	a := schema.StringAttribute{Computed: true, MarkdownDescription: description}
	if stable {
		a.PlanModifiers = []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	}
	return a
}

func definitionSchema(description string, replace bool) schema.StringAttribute {
	a := schema.StringAttribute{Optional: true, MarkdownDescription: description, Validators: []validator.String{stringCheck("nonempty string")}}
	if replace {
		a.PlanModifiers = []planmodifier.String{stringplanmodifier.RequiresReplace()}
	}
	return a
}

func source(ctx context.Context, configuration, channels, system types.String, files types.Map) (guix.Source, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	s := guix.Source{Configuration: configuration.ValueString(), Channels: channels.ValueString(), System: system.ValueString()}
	if s.Configuration == "" || s.Channels == "" {
		diagnostics.AddError("Missing Guix source", "configuration and pinned channels are required to create or update a host.")
	}
	if !files.IsNull() {
		diagnostics.Append(files.ElementsAs(ctx, &s.Files, false)...)
	}
	for name := range s.Files {
		if !guix.ValidSourcePath(name) {
			diagnostics.AddError("Invalid source file", fmt.Sprintf("Invalid or reserved relative path %q.", name))
		}
	}
	return s, diagnostics
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
