package provider

import (
	"context"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*clusterDataSource)(nil)

// NewClusterDataSource returns the k3d_cluster data source.
func NewClusterDataSource() datasource.DataSource {
	return &clusterDataSource{}
}

type clusterDataSource struct{}

type clusterDataModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	Image                types.String `tfsdk:"image"`
	Servers              types.Int64  `tfsdk:"servers"`
	Agents               types.Int64  `tfsdk:"agents"`
	API                  types.Object `tfsdk:"api"`
	Network              types.String `tfsdk:"network"`
	Token                types.String `tfsdk:"token"`
	Ports                types.Set    `tfsdk:"ports"`
	Kubeconfig           types.String `tfsdk:"kubeconfig"`
	Host                 types.String `tfsdk:"host"`
	ClientCertificate    types.String `tfsdk:"client_certificate"`
	ClientKey            types.String `tfsdk:"client_key"`
	ClusterCACertificate types.String `tfsdk:"cluster_ca_certificate"`
}

func (d *clusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (d *clusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing k3d cluster.",
		Attributes: map[string]schema.Attribute{
			"id":      schema.StringAttribute{MarkdownDescription: "The cluster name.", Computed: true},
			"name":    schema.StringAttribute{MarkdownDescription: "Cluster name.", Required: true},
			"image":   schema.StringAttribute{MarkdownDescription: "k3s image of the server nodes.", Computed: true},
			"servers": schema.Int64Attribute{MarkdownDescription: "Number of server nodes.", Computed: true},
			"agents":  schema.Int64Attribute{MarkdownDescription: "Number of agent nodes.", Computed: true},
			"api": schema.SingleNestedAttribute{
				MarkdownDescription: "How the Kubernetes API is exposed.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"host":    schema.StringAttribute{MarkdownDescription: "Host name used in the kubeconfig.", Computed: true},
					"host_ip": schema.StringAttribute{MarkdownDescription: "Host IP the API listens on.", Computed: true},
					"port":    schema.Int64Attribute{MarkdownDescription: "Host port the API listens on.", Computed: true},
				},
			},
			"network": schema.StringAttribute{MarkdownDescription: "Docker network of the cluster.", Computed: true},
			"token":   schema.StringAttribute{MarkdownDescription: "Cluster join token.", Computed: true, Sensitive: true},
			"ports": schema.SetNestedAttribute{
				MarkdownDescription: "Host ports mapped through the cluster load balancer.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"host_ip":        schema.StringAttribute{MarkdownDescription: "Host IP, null for all interfaces.", Computed: true},
						"host_port":      schema.Int64Attribute{MarkdownDescription: "Port on the Docker host.", Computed: true},
						"container_port": schema.Int64Attribute{MarkdownDescription: "Port on the nodes.", Computed: true},
						"protocol":       schema.StringAttribute{MarkdownDescription: "Protocol, null for tcp.", Computed: true},
					},
				},
			},
			"kubeconfig":             schema.StringAttribute{MarkdownDescription: "Kubeconfig for the cluster.", Computed: true, Sensitive: true},
			"host":                   schema.StringAttribute{MarkdownDescription: "Kubernetes API URL.", Computed: true},
			"client_certificate":     schema.StringAttribute{MarkdownDescription: "PEM-encoded client certificate.", Computed: true, Sensitive: true},
			"client_key":             schema.StringAttribute{MarkdownDescription: "PEM-encoded client key.", Computed: true, Sensitive: true},
			"cluster_ca_certificate": schema.StringAttribute{MarkdownDescription: "PEM-encoded cluster CA certificate.", Computed: true, Sensitive: true},
		},
	}
}

func (d *clusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.GetCluster(ctx, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster", err.Error())
		return
	}

	var full clusterModel
	full.Ports = types.SetNull(types.ObjectType{AttrTypes: portAttrTypes})
	resp.Diagnostics.Append(applyClusterState(ctx, &full, state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, clusterDataModel{
		ID:                   full.ID,
		Name:                 full.Name,
		Image:                full.Image,
		Servers:              full.Servers,
		Agents:               full.Agents,
		API:                  full.API,
		Network:              full.Network,
		Token:                full.Token,
		Ports:                full.Ports,
		Kubeconfig:           full.Kubeconfig,
		Host:                 full.Host,
		ClientCertificate:    full.ClientCertificate,
		ClientKey:            full.ClientKey,
		ClusterCACertificate: full.ClusterCACertificate,
	})...)
}
