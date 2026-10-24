// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/crypto/ssh"
)

type KeyPairResource struct{}
type KeyPairModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	PublicKey     types.String `tfsdk:"public_key"`
	AuthorizedKey types.String `tfsdk:"authorized_key"`
	Fingerprint   types.String `tfsdk:"fingerprint"`
}

func NewKeyPairResource() resource.Resource { return &KeyPairResource{} }
func (*KeyPairResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_key_pair"
}
func (*KeyPairResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "A named management public key for Guix configurations. Pass authorized_key into openssh-service-type authorized-keys and use the matching private key for management. This logical resource does not contact hosts; revocation takes effect when consumers are redeployed.", Attributes: map[string]schema.Attribute{
		"id":             computedString("Identity derived from the name and canonical public key.", false),
		"name":           schema.StringAttribute{Required: true, MarkdownDescription: "Logical key name.", Validators: []validator.String{stringCheck("nonempty string")}},
		"public_key":     schema.StringAttribute{Required: true, MarkdownDescription: "One OpenSSH management public key, such as tls_private_key.management.public_key_openssh."},
		"authorized_key": computedString("Canonical OpenSSH public key without comments or authorization options.", false),
		"fingerprint":    computedString("SHA256 OpenSSH public key fingerprint.", false),
	}}
}

func keyPair(model *KeyPairModel) error {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(model.PublicKey.ValueString()))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("public_key must contain exactly one OpenSSH public key without authorization options")
	}
	if _, certificate := key.(*ssh.Certificate); certificate {
		return fmt.Errorf("SSH certificates are not supported as management keys")
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	model.AuthorizedKey = types.StringValue(canonical)
	model.Fingerprint = types.StringValue(ssh.FingerprintSHA256(key))
	model.ID = types.StringValue(fmt.Sprintf("%x", sha256.Sum256([]byte(model.Name.ValueString()+"\x00"+canonical))))
	return nil
}

func (*KeyPairResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m KeyPairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := keyPair(&m); err != nil {
		resp.Diagnostics.AddError("Invalid public key", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
func (*KeyPairResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m KeyPairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := keyPair(&m); err != nil {
		resp.Diagnostics.AddError("Invalid public key", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
func (*KeyPairResource) Read(context.Context, resource.ReadRequest, *resource.ReadResponse)       {}
func (*KeyPairResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
