package provider

import (
	"context"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// harnessProvider adds a test-only data source to the real provider. Terraform
// only configures a provider that something in the configuration uses, so
// without it the provider block is never validated or configured.
type harnessProvider struct {
	*K3dProvider
}

func (p *harnessProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		func() datasource.DataSource { return &harnessDataSource{} },
	}
}

// harnessDataSource exposes the Config the provider passed to it.
type harnessDataSource struct {
	cfg *Config
}

type harnessModel struct {
	DockerHost types.String `tfsdk:"docker_host"`
}

func (d *harnessDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_harness"
}

func (d *harnessDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"docker_host": schema.StringAttribute{Computed: true},
		},
	}
}

func (d *harnessDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	cfg, ok := req.ProviderData.(*Config)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", "expected *Config")
		return
	}
	d.cfg = cfg
}

func (d *harnessDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	resp.Diagnostics.Append(resp.State.Set(ctx, harnessModel{DockerHost: types.StringValue(d.cfg.DockerHost)})...)
}

var testAccHarnessFactories = map[string]func() (tfprotov6.ProviderServer, error){
	typeName: providerserver.NewProtocol6WithError(&harnessProvider{K3dProvider: &K3dProvider{version: "test"}}),
}

var testAccVersionChecks = []tfversion.TerraformVersionCheck{
	tfversion.SkipBelow(tfversion.Version1_11_0),
}

func TestAccProvider_dockerHost(t *testing.T) {
	const host = "unix:///var/run/docker.sock"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccHarnessFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps: []resource.TestStep{
			{
				Config: `provider "k3d" {
  docker_host = "not-a-docker-host"
}

data "k3d_harness" "test" {}
`,
				ExpectError: regexp.MustCompile(`must start with unix://, tcp://, or ssh://`),
			},
			{
				Config: `provider "k3d" {
  docker_host = "` + host + `"
}

data "k3d_harness" "test" {}
`,
				Check: resource.TestCheckResourceAttr("data.k3d_harness.test", "docker_host", host),
			},
		},
	})
}
