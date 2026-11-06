// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var _ resource.ResourceWithConfigure = &HostResource{}
var _ resource.ResourceWithModifyPlan = &HostResource{}
var _ resource.ResourceWithImportState = &HostResource{}

type HostResource struct{ client *Client }
type HostModel struct {
	ID                   types.String `tfsdk:"id"`
	Hostname             types.String `tfsdk:"hostname"`
	Blueprint            types.Object `tfsdk:"blueprint"`
	System               types.String `tfsdk:"system"`
	SSH                  types.Object `tfsdk:"ssh"`
	Timeouts             types.Object `tfsdk:"timeouts"`
	IPv4                 types.String `tfsdk:"ipv4_address"`
	SystemPath           types.String `tfsdk:"system_path"`
	DeployedSystem       types.String `tfsdk:"deployed_system"`
	DeploymentRevision   types.String `tfsdk:"deployment_revision"`
	ProvisioningRevision types.String `tfsdk:"provisioning_revision"`
}

type TimeoutsModel struct {
	Create types.String `tfsdk:"create"`
	Read   types.String `tfsdk:"read"`
	Update types.String `tfsdk:"update"`
}

func NewHostResource() resource.Resource { return &HostResource{} }
func (*HostResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host"
}
func (*HostResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{}
	attrs["blueprint"] = blueprintSchema()
	attrs["system"] = systemSchema(false)
	attrs["id"] = computedString("Stable hostname-based identity.", true)
	attrs["hostname"] = schema.StringAttribute{Required: true, MarkdownDescription: "mDNS hostname ending in .local. Changing it replaces the tracked host.", Validators: []validator.String{stringCheck("mDNS hostname ending in .local")}, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}}
	attrs["ssh"] = sshSchema()
	attrs["timeouts"] = schema.SingleNestedAttribute{Optional: true, MarkdownDescription: "Operation deadlines. Defaults: create 45m, read 30s, update 30m. An unreachable host is absent after the read deadline.", Attributes: map[string]schema.Attribute{
		"create": schema.StringAttribute{Optional: true, MarkdownDescription: "Commissioning and initial reconciliation timeout.", Validators: []validator.String{stringCheck("positive duration")}},
		"read":   schema.StringAttribute{Optional: true, MarkdownDescription: "Reachability and refresh timeout.", Validators: []validator.String{stringCheck("positive duration")}},
		"update": schema.StringAttribute{Optional: true, MarkdownDescription: "Deployment and verification timeout.", Validators: []validator.String{stringCheck("positive duration")}},
	}}
	attrs["ipv4_address"] = computedString("Currently discovered IPv4 address. Unknown during configuration changes because deployment can change networking.", false)
	attrs["system_path"] = computedString("Currently observed /run/current-system store path.", false)
	attrs["deployed_system"] = computedString("Last successfully reconciled system store path. Retained during refresh to detect rollback drift.", false)
	attrs["deployment_revision"] = computedString("Source digest of the last successful reconciliation.", false)
	attrs["provisioning_revision"] = schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "Private digest of the last successfully installed file contents, modes and provisioner implementation. Secret-only updates install files without reconfiguring the system."}
	resp.Schema = schema.Schema{MarkdownDescription: "A reachable Guix installation discovered via mDNS. Create waits for manual flashing/boot and reconciles the desired system; updates use pinned guix deploy. Unreachable hosts are removed from state after the read timeout. Destroy only forgets the host.", Attributes: attrs}
}
func (r *HostResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	var err error
	r.client, err = configureClient(req.ProviderData)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
	}
}

func hostTimeout(ctx context.Context, object types.Object, operation string) (time.Duration, diag.Diagnostics) {
	d := map[string]time.Duration{"create": 45 * time.Minute, "read": 30 * time.Second, "update": 30 * time.Minute}[operation]
	var diagnostics diag.Diagnostics
	if object.IsNull() {
		return d, diagnostics
	}
	var m TimeoutsModel
	diagnostics.Append(object.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	v := map[string]types.String{"create": m.Create, "read": m.Read, "update": m.Update}[operation]
	if !v.IsNull() {
		parsed, err := time.ParseDuration(v.ValueString())
		if err != nil || parsed <= 0 {
			diagnostics.AddError("Invalid timeout", "Expected a positive duration for "+operation+".")
		} else {
			d = parsed
		}
	}
	return d, diagnostics
}

// Wait retries only reachability failures; authentication, host keys, malformed
// responses and broken tooling are surfaced immediately without discarding state.
func (r *HostResource) wait(ctx context.Context, hostname string, d guix.Destination, expected string) (guix.Destination, string, error) {
	for {
		if ctx.Err() != nil {
			return d, "", fmt.Errorf("%w: %s", discovery.ErrUnavailable, hostname)
		}
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		address, err := r.client.Resolver.Resolve(attempt, hostname)
		var observed string
		if err == nil {
			d.Address = address
			observed, err = r.client.Inspector.Inspect(attempt, d)
		}
		cancel()
		if err == nil && (expected == "" || observed == expected) {
			return d, observed, nil
		}
		if err != nil && !errors.Is(err, discovery.ErrUnavailable) {
			return d, "", err
		}
		if err == nil && expected != "" {
			tflog.Debug(ctx, "Waiting for deployed Guix system", map[string]any{"observed": observed, "expected": expected})
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if expected != "" {
				return d, "", fmt.Errorf("deployment verification timed out waiting for %s", expected)
			}
			return d, "", fmt.Errorf("%w: %s", discovery.ErrUnavailable, hostname)
		case <-timer.C:
		}
	}
}

func (r *HostResource) reconcile(ctx context.Context, model *HostModel, operation string, cachedSystem ...string) diag.Diagnostics {
	d, diagnostics := destination(ctx, model.SSH, r.client.SSH)
	s, known, sourceDiagnostics := blueprintSource(ctx, model.Blueprint, model.System, "machine")
	diagnostics.Append(sourceDiagnostics...)
	files, secretsKnown, secretDiagnostics := blueprintSecretFiles(ctx, model.Blueprint)
	diagnostics.Append(secretDiagnostics...)
	if (!known || !secretsKnown) && !diagnostics.HasError() {
		diagnostics.AddError("Unknown Guix host inputs", "The selected blueprint specification, channels, source files and system must be known before reconciliation.")
	}
	duration, timeoutDiagnostics := hostTimeout(ctx, model.Timeouts, operation)
	diagnostics.Append(timeoutDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	tflog.Info(ctx, "Waiting for manually commissioned Guix host", map[string]any{
		"hostname": model.Hostname.ValueString(), "instructions": "Boot a Guix System installation with mDNS and the configured SSH access.",
		"timeout": duration.String(),
	})
	d, observed, err := r.wait(ctx, model.Hostname.ValueString(), d, "")
	if err != nil {
		diagnostics.AddError("Guix host unavailable", err.Error())
		return diagnostics
	}
	expected := ""
	if len(cachedSystem) != 0 {
		expected = cachedSystem[0]
	}
	if expected == "" {
		expected, err = r.client.Guix.BuildSystem(ctx, s)
		if err != nil {
			diagnostics.AddError("Build desired Guix system", err.Error())
			return diagnostics
		}
	}
	if err := r.client.Guix.ProvisionHost(ctx, s.Channels, files, d); err != nil {
		diagnostics.AddError("Guix secret provisioning failed", err.Error())
		return diagnostics
	}
	if observed != expected {
		if err := r.client.Guix.Deploy(ctx, s, d); err != nil {
			diagnostics.AddError("Guix deployment failed", err.Error())
			return diagnostics
		}
		d, observed, err = r.wait(ctx, model.Hostname.ValueString(), d, expected)
		if err != nil {
			diagnostics.AddError("Verify Guix deployment", err.Error())
			return diagnostics
		}
	}
	model.ID = types.StringValue(model.Hostname.ValueString())
	model.IPv4, model.SystemPath = types.StringValue(d.Address), types.StringValue(observed)
	model.DeployedSystem, model.DeploymentRevision = types.StringValue(expected), types.StringValue(s.Digest())
	model.ProvisioningRevision = types.StringValue(files.Revision())
	return diagnostics
}

func (r *HostResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model HostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.reconcile(ctx, &model, "create")...)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	}
}
func (r *HostResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var model, old HostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if hostNeedsReconcile(ctx, model, old) {
		cached := ""
		if s, known, diagnostics := blueprintSource(ctx, model.Blueprint, model.System, "machine"); known && !diagnostics.HasError() && s.Digest() == old.DeploymentRevision.ValueString() {
			cached = old.DeployedSystem.ValueString()
		}
		resp.Diagnostics.Append(r.reconcile(ctx, &model, "update", cached)...)
	}
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	}
}
func (r *HostResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var model HostModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, diagnostics := destination(ctx, model.SSH, r.client.SSH)
	resp.Diagnostics.Append(diagnostics...)
	duration, diagnostics := hostTimeout(ctx, model.Timeouts, "read")
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	d, observed, err := r.wait(readCtx, model.Hostname.ValueString(), d, "")
	if ctx.Err() != nil {
		resp.Diagnostics.AddError("Host refresh cancelled", ctx.Err().Error())
		return
	}
	if errors.Is(err, discovery.ErrUnavailable) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read Guix host", err.Error())
		return
	}
	model.IPv4, model.SystemPath = types.StringValue(d.Address), types.StringValue(observed)
	if model.DeployedSystem.IsNull() {
		model.DeployedSystem, model.DeploymentRevision = types.StringValue(observed), types.StringValue("imported")
		model.ProvisioningRevision = types.StringValue("")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
func (*HostResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var model HostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, _, diagnostics := blueprintSource(ctx, model.Blueprint, model.System, "machine")
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	if req.State.Raw.IsNull() {
		target := "the configured .local hostname"
		if !model.Hostname.IsNull() && !model.Hostname.IsUnknown() {
			target = fmt.Sprintf("%q", model.Hostname.ValueString())
		}
		resp.Diagnostics.AddWarning("Manual commissioning required", "Boot a Guix System installation. Ensure "+target+" is discoverable over mDNS and accepts SSH with the configured management key and pinned host key. Apply waits for this host until the create timeout, then reconciles its blueprint. Image builds are independent; add an explicit depends_on if bootstrap ordering is required.")
		return
	}
	var old HostModel
	resp.Diagnostics.Append(req.State.Get(ctx, &old)...)
	if resp.Diagnostics.HasError() {
		return
	}
	needsReconcile := hostNeedsReconcile(ctx, model, old)
	inputsChanged := hostPublicInputsChanged(ctx, model, old)
	for name, value := range map[string]types.String{"ipv4_address": old.IPv4, "system_path": old.SystemPath, "deployed_system": old.DeployedSystem, "deployment_revision": old.DeploymentRevision, "provisioning_revision": old.ProvisioningRevision} {
		if needsReconcile && (name != "ipv4_address" || inputsChanged) {
			// This also requests reconciliation after an external rollback, when
			// the only state change is a computed observation.
			value = types.StringUnknown()
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), value)...)
	}
}

func hostNeedsReconcile(ctx context.Context, plan, old HostModel) bool {
	return hostInputsChanged(ctx, plan, old) || !old.SystemPath.Equal(old.DeployedSystem)
}

func hostInputsChanged(ctx context.Context, plan, old HostModel) bool {
	files, secretsKnown, secretDiagnostics := blueprintSecretFiles(ctx, plan.Blueprint)
	return hostPublicInputsChanged(ctx, plan, old) || secretDiagnostics.HasError() || !secretsKnown || old.ProvisioningRevision.ValueString() != files.Revision()
}

func hostPublicInputsChanged(ctx context.Context, plan, old HostModel) bool {
	s, known, diagnostics := blueprintSource(ctx, plan.Blueprint, plan.System, "machine")
	return diagnostics.HasError() || !known || !plan.SSH.Equal(old.SSH) || old.DeploymentRevision.ValueString() != s.Digest()
}
func (*HostResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
func (*HostResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !discovery.ValidHostname(req.ID) {
		resp.Diagnostics.AddError("Invalid import ID", "Use an mDNS hostname ending in .local.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("hostname"), req.ID)...)
}
