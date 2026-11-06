package provider

import (
	"context"
	"errors"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultRegistryHostIP = "0.0.0.0"

var (
	_ resource.Resource                = (*registryResource)(nil)
	_ resource.ResourceWithImportState = (*registryResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*registryResource)(nil)
)

// NewRegistryResource returns the k3d_registry resource.
func NewRegistryResource() resource.Resource {
	return &registryResource{}
}

type registryResource struct{}

type registryModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Image         types.String `tfsdk:"image"`
	HostIP        types.String `tfsdk:"host_ip"`
	Port          types.Int64  `tfsdk:"port"`
	Network       types.String `tfsdk:"network"`
	Volumes       types.List   `tfsdk:"volumes"`
	DeleteEnabled types.Bool   `tfsdk:"delete_enabled"`
	Proxy         types.Object `tfsdk:"proxy"`
	NodeName      types.String `tfsdk:"node_name"`
	Ref           types.String `tfsdk:"ref"`
}

type proxyModel struct {
	RemoteURL types.String `tfsdk:"remote_url"`
	Username  types.String `tfsdk:"username"`
	Password  types.String `tfsdk:"password"`
}

func (r *registryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry"
}

func (r *registryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A k3d container registry. Every change replaces the registry, and images stored in it are lost unless `volumes` persists them. " +
			"Create it before the clusters that use it by passing `ref` to `k3d_cluster.registries.use`.",
		Attributes: map[string]schema.Attribute{
			"id": computedString("The registry name.", false),
			"name": schema.StringAttribute{
				MarkdownDescription: "Registry name, without the `k3d-` prefix.",
				Required:            true,
				Validators:          []validator.String{clusterName()},
			},
			"image": schema.StringAttribute{
				MarkdownDescription: "Registry image with a pinned tag or digest.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(k3dclient.DefaultRegistryImage),
				Validators:          []validator.String{pinnedImage()},
			},
			"host_ip": schema.StringAttribute{
				MarkdownDescription: "Host IP to bind. Defaults to all interfaces.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultRegistryHostIP),
			},
			"port": schema.Int64Attribute{
				MarkdownDescription: "Port on the Docker host.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.Between(1, 65535)},
			},
			"network": schema.StringAttribute{
				MarkdownDescription: "Docker network to join when created. Clusters that use the registry connect it to their own network.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(k3dclient.DefaultRegistryNetwork),
			},
			"volumes": schema.ListAttribute{
				MarkdownDescription: "Volumes for the registry, such as `/data/registry:/var/lib/registry`. Sources are on the Docker host.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"delete_enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the registry allows deleting images.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"proxy": schema.SingleNestedAttribute{
				MarkdownDescription: "Runs the registry as a pull-through cache.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"remote_url": schema.StringAttribute{MarkdownDescription: "URL of the upstream registry.", Required: true},
					"username":   schema.StringAttribute{MarkdownDescription: "Upstream username.", Optional: true},
					"password":   schema.StringAttribute{MarkdownDescription: "Upstream password. Docker stores it in the container environment.", Optional: true, Sensitive: true},
				},
			},
			"node_name": computedString("Name of the registry container, with the `k3d-` prefix.", false),
			"ref":       computedString("Reference for `k3d_cluster.registries.use`.", false),
		},
	}
}

// ModifyPlan forces replacement on any change, since k3d cannot edit a registry.
func (r *registryResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var state, plan registryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	imported, diags := isImported(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.RequiresReplace = append(resp.RequiresReplace, replacementPaths([]createOnly{
		{"name", state.Name, plan.Name},
		{"image", state.Image, plan.Image},
		{"host_ip", state.HostIP, plan.HostIP},
		{"port", state.Port, plan.Port},
		{"network", state.Network, plan.Network},
		{"volumes", state.Volumes, plan.Volumes},
		{"delete_enabled", state.DeleteEnabled, plan.DeleteEnabled},
		{"proxy", state.Proxy, plan.Proxy},
	}, imported)...)
}

func (r *registryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan registryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec, diags := registrySpec(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.CreateRegistry(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create registry", err.Error())
		return
	}

	applyRegistryState(&plan, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *registryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data registryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.GetRegistry(ctx, data.ID.ValueString())
	if errors.Is(err, k3dclient.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read registry", err.Error())
		return
	}

	applyRegistryState(&data, state)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// Update only runs after an import, to record adopted configuration in state.
func (r *registryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan registryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.GetRegistry(ctx, plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read registry", err.Error())
		return
	}

	applyRegistryState(&plan, state)
	resp.Diagnostics.Append(clearImported(ctx, resp.Private)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *registryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state registryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := k3dclient.DeleteRegistry(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to delete registry", err.Error())
	}
}

// ImportState imports a registry by name, without the k3d- prefix.
func (r *registryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(markImported(ctx, resp.Private)...)
}

func applyRegistryState(data *registryModel, state *k3dclient.RegistryState) {
	data.ID = types.StringValue(state.Name)
	data.Name = types.StringValue(state.Name)
	data.Image = types.StringValue(state.Image)
	data.Port = types.Int64Value(int64(state.Port))
	data.NodeName = types.StringValue(state.NodeName)
	data.Ref = types.StringValue(state.Ref)
	if state.HostIP != "" {
		data.HostIP = types.StringValue(state.HostIP)
	}
}

func registrySpec(ctx context.Context, m registryModel) (k3dclient.RegistrySpec, diag.Diagnostics) {
	volumes, diags := stringList(ctx, m.Volumes)

	spec := k3dclient.RegistrySpec{
		Name:          m.Name.ValueString(),
		Image:         m.Image.ValueString(),
		HostIP:        m.HostIP.ValueString(),
		Port:          int(m.Port.ValueInt64()),
		Network:       m.Network.ValueString(),
		Volumes:       volumes,
		DeleteEnabled: m.DeleteEnabled.ValueBool(),
	}

	if !m.Proxy.IsNull() {
		var proxy proxyModel
		diags.Append(m.Proxy.As(ctx, &proxy, objectAsOptions())...)
		spec.ProxyRemoteURL = proxy.RemoteURL.ValueString()
		spec.ProxyUsername = proxy.Username.ValueString()
		spec.ProxyPassword = proxy.Password.ValueString()
	}

	return spec, diags
}
