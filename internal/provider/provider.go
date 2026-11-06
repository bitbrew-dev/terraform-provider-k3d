// Package provider implements the k3d Terraform provider.
package provider

import (
	"context"
	"os"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	typeName      = "k3d"
	dockerHostEnv = "DOCKER_HOST"
)

var dockerHostPattern = regexp.MustCompile(`^(unix|tcp|ssh)://.+`)

var _ provider.Provider = (*K3dProvider)(nil)

// K3dProvider is the Terraform provider for k3d clusters and their workloads.
type K3dProvider struct {
	version string
}

// Config is the resolved provider configuration shared with resources and data sources.
type Config struct {
	// DockerHost is the Docker endpoint. Empty means the Docker client default.
	DockerHost string
}

type providerModel struct {
	DockerHost types.String `tfsdk:"docker_host"`
}

// New returns a constructor for the provider at the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &K3dProvider{version: version}
	}
}

// Metadata returns the provider type name and version.
func (p *K3dProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = typeName
	resp.Version = p.version
}

// Schema defines the provider configuration.
func (p *K3dProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages k3d clusters and the workloads that run on them.",
		Attributes: map[string]schema.Attribute{
			"docker_host": schema.StringAttribute{
				MarkdownDescription: "Docker endpoint, using `unix://`, `tcp://`, or `ssh://`. " +
					"Falls back to the `DOCKER_HOST` environment variable, then the Docker client default.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(dockerHostPattern, "must start with unix://, tcp://, or ssh:// and include an address"),
				},
			},
		},
	}
}

// Configure resolves the provider configuration for resources and data sources.
func (p *K3dProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.DockerHost.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("docker_host"),
			"Unknown Docker host",
			"docker_host cannot come from a resource created in the same apply, because data sources need the Docker endpoint during refresh.",
		)
		return
	}

	dockerHost := data.DockerHost.ValueString()
	if data.DockerHost.IsNull() {
		dockerHost = os.Getenv(dockerHostEnv)
	}

	cfg := &Config{DockerHost: dockerHost}
	resp.DataSourceData = cfg
	resp.ResourceData = cfg
}

// Resources returns the resources the provider supports.
func (p *K3dProvider) Resources(_ context.Context) []func() resource.Resource {
	return nil
}

// DataSources returns the data sources the provider supports.
func (p *K3dProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
