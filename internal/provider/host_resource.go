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
	ID                 types.String `tfsdk:"id"`
	Hostname           types.String `tfsdk:"hostname"`
	Image              types.Object `tfsdk:"image"`
	SSH                types.Object `tfsdk:"ssh"`
	Timeouts           types.Object `tfsdk:"timeouts"`
	IPv4               types.String `tfsdk:"ipv4_address"`
	SystemPath         types.String `tfsdk:"system_path"`
	DeployedSystem     types.String `tfsdk:"deployed_system"`
	DeploymentRevision types.String `tfsdk:"deployment_revision"`
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
	attrs["image"] = hostImageSchema()
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

func (r *HostResource) reconcile(ctx context.Context, model *HostModel, operation string) diag.Diagnostics {
	d, diagnostics := destination(ctx, model.SSH, r.client.SSH)
	image, imageDiagnostics := readHostImage(ctx, model.Image)
	diagnostics.Append(imageDiagnostics...)
	s, sourceDiagnostics := source(ctx, image.Configuration, image.Channels, image.System, image.SourceFiles)
	s.SystemDefinition = image.SystemDefinition.ValueString()
	diagnostics.Append(sourceDiagnostics...)
	duration, timeoutDiagnostics := hostTimeout(ctx, model.Timeouts, operation)
	diagnostics.Append(timeoutDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	tflog.Info(ctx, "Waiting for manually commissioned Guix host", map[string]any{
		"hostname": model.Hostname.ValueString(), "image_id": image.ID.ValueString(),
		"image_path": image.ArtifactPath.ValueString(), "instructions": commissioningImageInstruction(image.ArtifactPath),
		"timeout": duration.String(),
	})
	d, observed, err := r.wait(ctx, model.Hostname.ValueString(), d, "")
	if err != nil {
		diagnostics.AddError("Guix host unavailable", err.Error())
		return diagnostics
	}
	expected, err := r.client.Guix.BuildSystem(ctx, s)
	if err != nil {
		diagnostics.AddError("Build desired Guix system", err.Error())
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
	var model HostModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.reconcile(ctx, &model, "update")...)
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
	image, diagnostics := readHostImage(ctx, model.Image)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	if req.State.Raw.IsNull() {
		instruction := commissioningImageInstruction(image.ArtifactPath)
		target := "the configured .local hostname"
		if !model.Hostname.IsNull() && !model.Hostname.IsUnknown() {
			target = fmt.Sprintf("%q", model.Hostname.ValueString())
		}
		resp.Diagnostics.AddWarning("Manual commissioning required", instruction+"\n\nEnsure "+target+" is discoverable over mDNS and accepts SSH with the configured management key and pinned host key. Apply waits for this host until the create timeout, then reconciles its Guix system.")
		return
	}
	var observed, deployed types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("system_path"), &observed)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("deployed_system"), &deployed)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !observed.IsNull() && !deployed.IsNull() && !observed.Equal(deployed) {
		// Computed observations alone don't request Update. An unknown revision
		// explicitly creates a reconciliation diff after an external rollback.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("deployment_revision"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("system_path"), types.StringUnknown())...)
	}
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
