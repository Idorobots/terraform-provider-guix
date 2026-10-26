// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/SpartanWorks/terraform-provider-guix/internal/discovery"
	"github.com/SpartanWorks/terraform-provider-guix/internal/guix"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	testresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"golang.org/x/crypto/ssh"
)

// The upstream testing library retains TF_ACC_* names. Select OpenTofu and its
// registry by default so direct TF_ACC=1 go test invocations match make testacc.
func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC") != "" {
		defaults := map[string]string{
			"TF_ACC_PROVIDER_HOST":      "registry.opentofu.org",
			"TF_ACC_PROVIDER_NAMESPACE": "spartanworks",
		}
		if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" && os.Getenv("TF_ACC_TERRAFORM_VERSION") == "" {
			cli := os.Getenv("TOFU")
			if cli == "" {
				cli = "tofu"
			}
			binary, err := exec.LookPath(cli)
			if err != nil {
				fmt.Fprintln(os.Stderr, "OpenTofu is required for acceptance tests:", err)
				os.Exit(1)
			}
			binary, err = filepath.Abs(binary)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			defaults["TF_ACC_TERRAFORM_PATH"] = binary
		}
		for name, value := range defaults {
			if os.Getenv(name) == "" {
				if err := os.Setenv(name, value); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			}
		}
	}
	os.Exit(m.Run())
}

type fakeBackend struct {
	mu                                          sync.Mutex
	directory                                   string
	address, observed                           string
	addressAfterDeploy                          string
	deployments, builds, attempts, failAttempts int
	resolveError, inspectError                  error
	deploymentError                             error
}

func (f *fakeBackend) Resolve(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.resolveError != nil {
		return "", f.resolveError
	}
	if f.attempts <= f.failAttempts {
		return "", discovery.ErrUnavailable
	}
	return f.address, nil
}
func (f *fakeBackend) Inspect(context.Context, guix.Destination) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observed, f.inspectError
}
func desired(s guix.Source) string { return "/gnu/store/" + s.Digest()[:32] + "-system" }
func (*fakeBackend) BuildSystem(_ context.Context, s guix.Source) (string, error) {
	return desired(s), nil
}
func (f *fakeBackend) Deploy(_ context.Context, s guix.Source, _ guix.Destination) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployments++
	f.observed = desired(s)
	if f.addressAfterDeploy != "" {
		f.address = f.addressAfterDeploy
		f.addressAfterDeploy = ""
	}
	err := f.deploymentError
	f.deploymentError = nil
	return err
}
func (f *fakeBackend) BuildImage(ctx context.Context, s guix.Source, _, target, root string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	image := filepath.Join(f.directory, "image-"+s.ImageDigest(target))
	if err := os.WriteFile(image, []byte(s.Configuration), 0600); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return "", "", err
	}
	if err := os.Symlink(image, root); err != nil {
		return "", "", err
	}
	checksum, err := guix.Checksum(ctx, image)
	return image, checksum, err
}

func fakeClient(t *testing.T) (*Client, *fakeBackend) {
	t.Helper()
	f := &fakeBackend{directory: t.TempDir(), address: "192.0.2.1", observed: "/gnu/store/00000000000000000000000000000000-system"}
	return &Client{Guix: f, Resolver: f, Inspector: f, ArtifactDirectory: t.TempDir()}, f
}

func TestProviderSchema(t *testing.T) {
	server := providerserver.NewProtocol6(New("test")())()
	response, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range response.Diagnostics {
		if diagnostic.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatal(diagnostic.Detail)
		}
	}
	for _, name := range []string{"guix_image", "guix_host", "guix_key_pair"} {
		if response.ResourceSchemas[name] == nil {
			t.Fatalf("missing %s", name)
		}
	}
}

func hostState(t *testing.T, r *HostResource) tfsdk.State {
	t.Helper()
	var schemaResponse resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schemaResponse)
	schema := schemaResponse.Schema
	state := tfsdk.State{Schema: schema}
	sshType, ok := schema.Attributes["ssh"].GetType().(types.ObjectType)
	if !ok {
		t.Fatal("SSH schema is not an object")
	}
	model := HostModel{
		Image: testHostImage(t),
		ID:    types.StringValue("node.local"), Hostname: types.StringValue("node.local"),
		SSH:      types.ObjectNull(sshType.AttrTypes),
		Timeouts: types.ObjectValueMust(map[string]attr.Type{"create": types.StringType, "read": types.StringType, "update": types.StringType}, map[string]attr.Value{"create": types.StringNull(), "read": types.StringValue("5ms"), "update": types.StringNull()}),
		IPv4:     types.StringValue("192.0.2.1"), SystemPath: types.StringValue("old"), DeployedSystem: types.StringValue("desired"), DeploymentRevision: types.StringValue("digest"),
	}
	if diagnostics := state.Set(t.Context(), model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return state
}

func TestHostRefreshFailureSemantics(t *testing.T) {
	for _, tc := range []struct {
		name               string
		resolve, inspect   error
		absent, diagnostic bool
	}{
		{"offline", discovery.ErrUnavailable, nil, true, false},
		{"broken resolver", errors.New("Avahi daemon not running"), nil, false, true},
		{"host key mismatch", nil, errors.New("host key mismatch"), false, true},
		{"authentication failure", nil, errors.New("unable to authenticate"), false, true},
		{"reachable", nil, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, backend := fakeClient(t)
			client.SSH = types.ObjectValueMust(map[string]attr.Type{"user": types.StringType, "port": types.Int64Type, "host_key": types.StringType, "private_key": types.StringType, "identity_file": types.StringType}, map[string]attr.Value{
				"user": types.StringNull(), "port": types.Int64Null(), "host_key": types.StringValue("host key"), "private_key": types.StringValue("management key"), "identity_file": types.StringNull(),
			})
			backend.resolveError, backend.inspectError = tc.resolve, tc.inspect
			r := &HostResource{client: client}
			state := hostState(t, r)
			response := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
			if response.Diagnostics.HasError() != tc.diagnostic || response.State.Raw.IsNull() != tc.absent {
				t.Fatalf("absent=%v diagnostics=%v", response.State.Raw.IsNull(), response.Diagnostics)
			}
		})
	}
}

func factories(client *Client) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){"guix": providerserver.NewProtocol6WithError(&GuixProvider{version: "test", client: client})}
}

func TestAccHostLifecycleAndDependencies(t *testing.T) {
	client, backend := fakeClient(t)
	backend.failAttempts = 1
	config := func(configuration string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "fake host key", private_key = "fake management key" }
}
resource "guix_host" "controller" {
  hostname = "controller.local"
  image = {
    id = "controller-image"
    artifact_path = "/tmp/controller.img"
    configuration = %q
    channels = "pinned"
    system = "x86_64-linux"
  }
}
resource "guix_image" "worker" {
  configuration = guix_host.controller.ipv4_address
  channels = "pinned"
}
`, client.ArtifactDirectory, configuration)
	}
	check := func(deployments, builds int) testresource.TestCheckFunc {
		return func(_ *terraform.State) error {
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.deployments != deployments || backend.builds != builds {
				return fmt.Errorf("deployments=%d builds=%d; want %d %d", backend.deployments, backend.builds, deployments, builds)
			}
			return nil
		}
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config("v1"), Check: testresource.ComposeTestCheckFunc(testresource.TestCheckResourceAttr("guix_host.controller", "ipv4_address", "192.0.2.1"), check(1, 1))},
		{ResourceName: "guix_host.controller", ImportState: true, ImportStateId: "controller.local", ImportStateVerify: true, ImportStateVerifyIgnore: []string{"image", "deployment_revision"}},
		{PreConfig: func() { backend.mu.Lock(); backend.addressAfterDeploy = "192.0.2.4"; backend.mu.Unlock() }, Config: config("v2"), Check: testresource.ComposeTestCheckFunc(testresource.TestCheckResourceAttr("guix_host.controller", "ipv4_address", "192.0.2.4"), check(2, 2))},
		{PreConfig: func() {
			backend.mu.Lock()
			backend.observed = "/gnu/store/00000000000000000000000000000000-system"
			backend.mu.Unlock()
		}, Config: config("v2"), Check: check(3, 2)},
		{PreConfig: func() { backend.mu.Lock(); backend.address = "192.0.2.2"; backend.mu.Unlock() }, Config: config("v2"), Check: testresource.ComposeTestCheckFunc(testresource.TestCheckResourceAttr("guix_host.controller", "ipv4_address", "192.0.2.2"), check(3, 3))},
		{PreConfig: func() {
			roots, err := filepath.Glob(filepath.Join(client.ArtifactDirectory, "*", "*", "image"))
			if err != nil || len(roots) != 1 {
				t.Fatalf("image roots=%v err=%v", roots, err)
			}
			if err := os.Remove(roots[0]); err != nil {
				t.Fatal(err)
			}
		}, Config: config("v2"), Check: check(3, 4)},
	}})
}

func TestAccInterruptedHostCreate(t *testing.T) {
	client, backend := fakeClient(t)
	backend.deploymentError = errors.New("connection lost after activation")
	config := `provider "guix" {
  ssh = { host_key = "fake host key", private_key = "fake management key" }
}
resource "guix_host" "server" {
  hostname = "server.local"
  image = {
    id = "server-image"
    artifact_path = "/tmp/server.img"
    configuration = "desired"
    channels = "pinned"
    system = "x86_64-linux"
  }
}`
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config, ExpectError: regexp.MustCompile("connection lost after activation")},
		{Config: config, Check: func(_ *terraform.State) error {
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.deployments != 1 {
				return fmt.Errorf("retry repeated an already activated deployment: %d deployments", backend.deployments)
			}
			return nil
		}},
	}})
}

func TestAccHostImageLifecycle(t *testing.T) {
	client, backend := fakeClient(t)
	config := func(imageConfiguration, system string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "fake host key", private_key = "fake management key" }
}

resource "guix_image" "bootstrap" {
  configuration = %q
  channels = "pinned"
  system = %q
  source_files = { asset = "image asset" }
}
resource "guix_host" "server" {
  hostname = "server.local"
  image = guix_image.bootstrap
}
`, client.ArtifactDirectory, imageConfiguration, system)
	}
	check := func(configuration, channels, system string, files map[string]string, builds, deployments int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			host := state.RootModule().Resources["guix_host.server"].Primary.Attributes
			image := state.RootModule().Resources["guix_image.bootstrap"].Primary.Attributes
			want := desired(guix.Source{Configuration: configuration, Channels: channels, System: system, Files: files})
			if host["id"] != "server.local" || host["image.system"] != system || host["deployed_system"] != want || host["image.id"] != image["id"] || host["image.artifact_path"] != image["artifact_path"] {
				return fmt.Errorf("unexpected host image state: %v; desired system %s", host, want)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.builds != builds || backend.deployments != deployments {
				return fmt.Errorf("builds=%d deployments=%d; want %d %d", backend.builds, backend.deployments, builds, deployments)
			}
			return nil
		}
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config("image-v1", "aarch64-linux"), Check: check("image-v1", "pinned", "aarch64-linux", map[string]string{"asset": "image asset"}, 1, 1)},
		{Config: config("image-v2", "riscv64-linux"), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_host.server", plancheck.ResourceActionUpdate),
		}}, Check: check("image-v2", "pinned", "riscv64-linux", map[string]string{"asset": "image asset"}, 2, 2)},
		{Config: config("image-v3", "aarch64-linux"), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_host.server", plancheck.ResourceActionUpdate),
		}}, Check: check("image-v3", "pinned", "aarch64-linux", map[string]string{"asset": "image asset"}, 3, 3)},
	}})
}

func TestAccImageCrossTargetLifecycle(t *testing.T) {
	client, backend := fakeClient(t)
	config := func(target string) string {
		return fmt.Sprintf(`
provider "guix" {
  artifact_directory = %q
  ssh = { host_key = "fake host key", private_key = "fake management key" }
}
resource "guix_image" "bootstrap" {
  configuration = "system"
  channels = "pinned"
  system = "aarch64-linux"
  %s
}
resource "guix_host" "server" {
  hostname = "server.local"
  image = guix_image.bootstrap
}
`, client.ArtifactDirectory, target)
	}
	check := func(target string, builds, deployments int) testresource.TestCheckFunc {
		return func(state *terraform.State) error {
			image := state.RootModule().Resources["guix_image.bootstrap"].Primary.Attributes
			source := guix.Source{Configuration: "system", Channels: "pinned", System: "aarch64-linux"}
			if image["input_digest"] != source.ImageDigest(target) {
				return fmt.Errorf("incorrect image digest: %s", image["input_digest"])
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.builds != builds || backend.deployments != deployments {
				return fmt.Errorf("builds=%d deployments=%d; want %d %d", backend.builds, backend.deployments, builds, deployments)
			}
			return nil
		}
	}
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{
		{Config: config(""), Check: check("", 1, 1)},
		{Config: config(`target = "aarch64-linux-gnu"`), ConfigPlanChecks: testresource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("guix_image.bootstrap", plancheck.ResourceActionDestroyBeforeCreate),
		}}, Check: check("aarch64-linux-gnu", 2, 1)},
		{Config: config(`target = "arm-linux-gnueabihf"`), Check: check("arm-linux-gnueabihf", 3, 1)},
	}})
}

func TestAccKeyPair(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := fakeClient(t)
	config := fmt.Sprintf(`resource "guix_key_pair" "management" {
name = "management"
public_key = %q
}`, string(ssh.MarshalAuthorizedKey(key)))
	testresource.Test(t, testresource.TestCase{ProtoV6ProviderFactories: factories(client), Steps: []testresource.TestStep{{Config: config, Check: testresource.TestCheckResourceAttr("guix_key_pair.management", "fingerprint", ssh.FingerprintSHA256(key))}}})
}

func TestImageRootOwnership(t *testing.T) {
	client, backend := fakeClient(t)
	source := guix.Source{Configuration: "same", Channels: "pinned", System: "x86_64-linux"}
	first := filepath.Join(client.ArtifactDirectory, "11111111111111111111111111111111", "image")
	second := filepath.Join(client.ArtifactDirectory, "22222222222222222222222222222222", "image")
	_, _, err := backend.BuildImage(t.Context(), source, "efi-raw", "", first)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = backend.BuildImage(t.Context(), source, "efi-raw", "", second)
	if err != nil {
		t.Fatal(err)
	}
	r := &ImageResource{client: client}
	var schemaResponse resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schemaResponse)
	state := tfsdk.State{Schema: schemaResponse.Schema}
	model := ImageModel{ID: types.StringValue("11111111111111111111111111111111"), ArtifactPath: types.StringValue(first), Configuration: types.StringValue("same"), Channels: types.StringValue("pinned"), System: types.StringValue("x86_64-linux"), SourceFiles: types.MapNull(types.StringType), ImageType: types.StringValue("efi-raw"), StorePath: types.StringValue("unused"), SHA256: types.StringValue("unused"), InputDigest: types.StringValue("unused"), BuildTimeout: types.StringValue("2h")}
	if diagnostics := state.Set(t.Context(), model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	var response resource.DeleteResponse
	r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("released another resource's shared image: %v", err)
	}
	if _, err := os.Lstat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first root retained: %v", err)
	}
}
