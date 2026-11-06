package provider

import (
	"context"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*registryDataSource)(nil)

// NewRegistryDataSource returns the k3d_registry data source.
func NewRegistryDataSource() datasource.DataSource {
	return &registryDataSource{}
}

type registryDataSource struct{}

type registryDataModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Image    types.String `tfsdk:"image"`
	HostIP   types.String `tfsdk:"host_ip"`
	Port     types.Int64  `tfsdk:"port"`
	Networks types.List   `tfsdk:"networks"`
	NodeName types.String `tfsdk:"node_name"`
	Ref      types.String `tfsdk:"ref"`
}

func (d *registryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry"
}

func (d *registryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing k3d registry.",
		Attributes: map[string]schema.Attribute{
			"id":        schema.StringAttribute{MarkdownDescription: "The registry name.", Computed: true},
			"name":      schema.StringAttribute{MarkdownDescription: "Registry name, without the `k3d-` prefix.", Required: true},
			"image":     schema.StringAttribute{MarkdownDescription: "Registry image.", Computed: true},
			"host_ip":   schema.StringAttribute{MarkdownDescription: "Host IP the registry is bound to.", Computed: true},
			"port":      schema.Int64Attribute{MarkdownDescription: "Port on the Docker host.", Computed: true},
			"networks":  schema.ListAttribute{MarkdownDescription: "Docker networks the registry is connected to.", ElementType: types.StringType, Computed: true},
			"node_name": schema.StringAttribute{MarkdownDescription: "Name of the registry container.", Computed: true},
			"ref":       schema.StringAttribute{MarkdownDescription: "Reference for `k3d_cluster.registries.use`.", Computed: true},
		},
	}
}

func (d *registryDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data registryDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.GetRegistry(ctx, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read registry", err.Error())
		return
	}

	networks, diags := types.ListValueFrom(ctx, types.StringType, state.Networks)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, registryDataModel{
		ID:       types.StringValue(state.Name),
		Name:     types.StringValue(state.Name),
		Image:    types.StringValue(state.Image),
		HostIP:   types.StringValue(state.HostIP),
		Port:     types.Int64Value(int64(state.Port)),
		Networks: networks,
		NodeName: types.StringValue(state.NodeName),
		Ref:      types.StringValue(state.Ref),
	})...)
}
