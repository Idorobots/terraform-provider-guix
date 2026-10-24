// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/SpartanWorks/terraform-provider-guix/internal/remote"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = &GuixProvider{}

type GuixProvider struct {
	version string
	// Tests inject backends through the same contracts used by production.
	client *Client
}

type Client struct {
	Guix              guix.Backend
	Resolver          discovery.Resolver
	Inspector         remote.Inspector
	ArtifactDirectory string
	SSH               types.Object
}

type ProviderModel struct {
	ArtifactDirectory types.String `tfsdk:"artifact_directory"`
	SSH               types.Object `tfsdk:"ssh"`
}

func (p *GuixProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName, resp.Version = "guix", p.version
}

func (p *GuixProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Build Guix System images and reconcile manually commissioned hosts through mDNS and SSH. Requires a Linux runner, Guix, and Avahi.",
		Attributes: map[string]schema.Attribute{
			"artifact_directory": schema.StringAttribute{Optional: true, MarkdownDescription: "Persistent local directory for per-resource image GC roots. Defaults to .guix-artifacts under the OpenTofu working directory."},
			"ssh": schema.SingleNestedAttribute{Optional: true, MarkdownDescription: "Default SSH credentials, also used for hostname imports. Host resources may override the entire object.", Attributes: map[string]schema.Attribute{
				"user":          schema.StringAttribute{Optional: true, MarkdownDescription: "SSH user. Defaults to root; other users require passwordless sudo for Guix deployment."},
				"port":          schema.Int64Attribute{Optional: true, MarkdownDescription: "SSH port. Defaults to 22."},
				"host_key":      schema.StringAttribute{Required: true, MarkdownDescription: "Pinned host identity as one OpenSSH public key, distinct from the management key."},
				"private_key":   schema.StringAttribute{Optional: true, Sensitive: true, MarkdownDescription: "Unencrypted management private key, for example tls_private_key.management.private_key_openssh. Exactly one of private_key or identity_file is needed."},
				"identity_file": schema.StringAttribute{Optional: true, MarkdownDescription: "Path to an unencrypted management private key on the runner."},
			}},
		},
	}
}

func (p *GuixProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model ProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	dir := model.ArtifactDirectory.ValueString()
	if dir == "" {
		dir = ".guix-artifacts"
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		resp.Diagnostics.AddError("Invalid artifact directory", err.Error())
		return
	}
	client := p.client
	if client == nil {
		runner := guix.ExecRunner{}
		client = &Client{Guix: guix.Client{Runner: runner}, Resolver: discovery.Avahi{Runner: runner}, Inspector: remote.SSH{}}
	}
	// Copy rather than mutate a shared test client across provider instances.
	configured := *client
	configured.ArtifactDirectory, configured.SSH = absolute, model.SSH
	resp.ResourceData = &configured
}

func (*GuixProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewImageResource, NewHostResource, NewKeyPairResource}
}

func (*GuixProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &GuixProvider{version: version} }
}

func configureClient(data any) (*Client, error) {
	client, ok := data.(*Client)
	if !ok {
		return nil, fmt.Errorf("expected *provider.Client, got %T", data)
	}
	return client, nil
}
