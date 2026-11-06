package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultClusterTimeout = "10m"

var (
	_ resource.Resource                   = (*clusterResource)(nil)
	_ resource.ResourceWithImportState    = (*clusterResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*clusterResource)(nil)
	_ resource.ResourceWithValidateConfig = (*clusterResource)(nil)
)

// NewClusterResource returns the k3d_cluster resource.
func NewClusterResource() resource.Resource {
	return &clusterResource{}
}

type clusterResource struct{}

type clusterModel struct {
	ID                   types.String `tfsdk:"id"`
	Name                 types.String `tfsdk:"name"`
	Image                types.String `tfsdk:"image"`
	Servers              types.Int64  `tfsdk:"servers"`
	Agents               types.Int64  `tfsdk:"agents"`
	API                  types.Object `tfsdk:"api"`
	Network              types.String `tfsdk:"network"`
	Subnet               types.String `tfsdk:"subnet"`
	Token                types.String `tfsdk:"token"`
	Traefik              types.Bool   `tfsdk:"traefik"`
	Ports                types.Set    `tfsdk:"ports"`
	Volumes              types.Set    `tfsdk:"volumes"`
	K3sArgs              types.List   `tfsdk:"k3s_args"`
	NodeLabels           types.Set    `tfsdk:"node_labels"`
	RuntimeLabels        types.Set    `tfsdk:"runtime_labels"`
	Env                  types.Set    `tfsdk:"env"`
	HostAliases          types.List   `tfsdk:"host_aliases"`
	Registries           types.Object `tfsdk:"registries"`
	Timeout              types.String `tfsdk:"timeout"`
	Kubeconfig           types.String `tfsdk:"kubeconfig"`
	Host                 types.String `tfsdk:"host"`
	ClientCertificate    types.String `tfsdk:"client_certificate"`
	ClientKey            types.String `tfsdk:"client_key"`
	ClusterCACertificate types.String `tfsdk:"cluster_ca_certificate"`
}

type apiModel struct {
	Host   types.String `tfsdk:"host"`
	HostIP types.String `tfsdk:"host_ip"`
	Port   types.Int64  `tfsdk:"port"`
}

type portModel struct {
	HostIP        types.String `tfsdk:"host_ip"`
	HostPort      types.Int64  `tfsdk:"host_port"`
	ContainerPort types.Int64  `tfsdk:"container_port"`
	Protocol      types.String `tfsdk:"protocol"`
}

type volumeModel struct {
	Source      types.String `tfsdk:"source"`
	Destination types.String `tfsdk:"destination"`
	NodeFilters types.List   `tfsdk:"node_filters"`
}

type k3sArgModel struct {
	Arg         types.String `tfsdk:"arg"`
	NodeFilters types.List   `tfsdk:"node_filters"`
}

type labelModel struct {
	Key         types.String `tfsdk:"key"`
	Value       types.String `tfsdk:"value"`
	NodeFilters types.List   `tfsdk:"node_filters"`
}

type envModel struct {
	Name        types.String `tfsdk:"name"`
	Value       types.String `tfsdk:"value"`
	NodeFilters types.List   `tfsdk:"node_filters"`
}

type hostAliasModel struct {
	IP        types.String `tfsdk:"ip"`
	Hostnames types.List   `tfsdk:"hostnames"`
}

type registriesModel struct {
	Use    types.List   `tfsdk:"use"`
	Config types.String `tfsdk:"config"`
}

var apiAttrTypes = map[string]attr.Type{
	"host":    types.StringType,
	"host_ip": types.StringType,
	"port":    types.Int64Type,
}

var portAttrTypes = map[string]attr.Type{
	"host_ip":        types.StringType,
	"host_port":      types.Int64Type,
	"container_port": types.Int64Type,
	"protocol":       types.StringType,
}

func nodeFiltersAttribute() schema.ListAttribute {
	return schema.ListAttribute{
		MarkdownDescription: "k3d node filters such as `server:*` or `agent:0`. Defaults to all servers and agents.",
		ElementType:         types.StringType,
		Optional:            true,
	}
}

func computedString(description string, sensitive bool) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: description,
		Computed:            true,
		Sensitive:           sensitive,
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *clusterResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A k3d cluster. Changing anything except `ports` and `timeout` replaces the cluster. " +
			"`volumes`, `k3s_args`, `node_labels`, `runtime_labels`, `env`, `host_aliases`, `registries`, `subnet`, and `traefik` " +
			"cannot be read back from k3d, so changes made outside Terraform are not detected. " +
			"Import a cluster by name. After an import, those settings are adopted from your configuration instead of replacing the cluster, " +
			"but `ports` are read from the cluster, so declare the existing ones or the next apply removes them.",
		Attributes: map[string]schema.Attribute{
			"id": computedString("The cluster name.", false),
			"name": schema.StringAttribute{
				MarkdownDescription: "Cluster name.",
				Required:            true,
				Validators:          []validator.String{clusterName()},
			},
			"image": schema.StringAttribute{
				MarkdownDescription: "k3s image with a pinned tag or digest, such as `rancher/k3s:v1.32.4-k3s1`.",
				Required:            true,
				Validators:          []validator.String{pinnedImage()},
			},
			"servers": schema.Int64Attribute{
				MarkdownDescription: "Number of server nodes. Use an odd number: an even number of etcd members adds no fault tolerance.",
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(1),
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
			"agents": schema.Int64Attribute{
				MarkdownDescription: "Number of agent nodes.",
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(0),
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"api": schema.SingleNestedAttribute{
				MarkdownDescription: "How the Kubernetes API is exposed. On a remote Docker host, set `port`, because the default free port is chosen on the machine running Terraform, and set `host`, because the default `0.0.0.0` is only reachable locally.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				Attributes: map[string]schema.Attribute{
					"host": schema.StringAttribute{
						MarkdownDescription: "Host name used in the kubeconfig.",
						Optional:            true,
						Computed:            true,
						PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"host_ip": schema.StringAttribute{
						MarkdownDescription: "Host IP the API listens on.",
						Optional:            true,
						Computed:            true,
						PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"port": schema.Int64Attribute{
						MarkdownDescription: "Host port the API listens on.",
						Optional:            true,
						Computed:            true,
						PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
						Validators:          []validator.Int64{int64validator.Between(1, 65535)},
					},
				},
			},
			"network": schema.StringAttribute{
				MarkdownDescription: "Existing Docker network to join. Defaults to a new `k3d-<name>` network. The `host` network is not supported.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Validators:          []validator.String{stringvalidator.NoneOf("host")},
			},
			"subnet": schema.StringAttribute{
				MarkdownDescription: "Subnet for the cluster network, such as `172.28.0.0/16`.",
				Optional:            true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Cluster join token. Generated when not set.",
				Optional:            true,
				Computed:            true,
				Sensitive:           true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"traefik": schema.BoolAttribute{
				MarkdownDescription: "Whether k3s installs its bundled Traefik ingress controller.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"ports": schema.SetNestedAttribute{
				MarkdownDescription: "Host ports mapped through the cluster load balancer. Changed in place, with a brief interruption while k3d replaces the load balancer.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"host_ip": schema.StringAttribute{
							MarkdownDescription: "Host IP to bind. Defaults to all interfaces.",
							Optional:            true,
						},
						"host_port": schema.Int64Attribute{
							MarkdownDescription: "Port on the Docker host.",
							Required:            true,
							Validators:          []validator.Int64{int64validator.Between(1, 65535)},
						},
						"container_port": schema.Int64Attribute{
							MarkdownDescription: "Port on the nodes, such as a NodePort.",
							Required:            true,
							Validators:          []validator.Int64{int64validator.Between(1, 65535)},
						},
						"protocol": schema.StringAttribute{
							MarkdownDescription: "`tcp` or `udp`. Defaults to `tcp`.",
							Optional:            true,
							Validators:          []validator.String{stringvalidator.OneOf("tcp", "udp")},
						},
					},
				},
			},
			"volumes": schema.SetNestedAttribute{
				MarkdownDescription: "Volumes mounted into the nodes. `source` is a path on the Docker host or a named volume, and is not removed when the cluster is destroyed.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"source":       schema.StringAttribute{MarkdownDescription: "Path on the Docker host or volume name.", Required: true},
						"destination":  schema.StringAttribute{MarkdownDescription: "Path inside the node. May end in `:ro`.", Required: true},
						"node_filters": nodeFiltersAttribute(),
					},
				},
			},
			"k3s_args": schema.ListNestedAttribute{
				MarkdownDescription: "Extra arguments for k3s.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"arg":          schema.StringAttribute{MarkdownDescription: "Argument such as `--tls-san=example.com`.", Required: true},
						"node_filters": nodeFiltersAttribute(),
					},
				},
			},
			"node_labels":    labelsAttribute("Kubernetes labels for the nodes."),
			"runtime_labels": labelsAttribute("Docker labels for the node containers."),
			"env": schema.SetNestedAttribute{
				MarkdownDescription: "Environment variables for the node containers.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":         schema.StringAttribute{MarkdownDescription: "Variable name.", Required: true},
						"value":        schema.StringAttribute{MarkdownDescription: "Variable value.", Required: true},
						"node_filters": nodeFiltersAttribute(),
					},
				},
			},
			"host_aliases": schema.ListNestedAttribute{
				MarkdownDescription: "Entries added to `/etc/hosts` in the nodes and to CoreDNS.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"ip": schema.StringAttribute{MarkdownDescription: "IP address.", Required: true},
						"hostnames": schema.ListAttribute{
							MarkdownDescription: "Host names for the IP address.",
							ElementType:         types.StringType,
							Required:            true,
						},
					},
				},
			},
			"registries": schema.SingleNestedAttribute{
				MarkdownDescription: "Container registries for the cluster.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"use": schema.ListAttribute{
						MarkdownDescription: "Registries to connect, usually `k3d_registry.<name>.ref`.",
						ElementType:         types.StringType,
						Optional:            true,
					},
					"config": schema.StringAttribute{
						MarkdownDescription: "Contents of k3s `registries.yaml`, not a file path. Use `file()` to load one.",
						Optional:            true,
						Sensitive:           true,
					},
				},
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "How long to wait for creation, including image pulls. Defaults to `10m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultClusterTimeout),
				Validators:          []validator.String{duration()},
			},
			"kubeconfig":             computedString("Kubeconfig for the cluster. Holds an admin client key, so use an encrypted state backend.", true),
			"host":                   computedString("Kubernetes API URL.", false),
			"client_certificate":     computedString("PEM-encoded client certificate.", true),
			"client_key":             computedString("PEM-encoded client key.", true),
			"cluster_ca_certificate": computedString("PEM-encoded cluster CA certificate.", true),
		},
	}
}

func labelsAttribute(description string) schema.SetNestedAttribute {
	return schema.SetNestedAttribute{
		MarkdownDescription: description,
		Optional:            true,
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"key":          schema.StringAttribute{MarkdownDescription: "Label key.", Required: true},
				"value":        schema.StringAttribute{MarkdownDescription: "Label value.", Required: true},
				"node_filters": nodeFiltersAttribute(),
			},
		},
	}
}

// ValidateConfig reports problems that do not need Docker.
func (r *clusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data clusterModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !data.Servers.IsNull() && !data.Servers.IsUnknown() && data.Servers.ValueInt64()%2 == 0 {
		resp.Diagnostics.AddAttributeWarning(
			path.Root("servers"),
			"Even number of servers",
			fmt.Sprintf("%d etcd members tolerate no more failures than %d. Use an odd number.", data.Servers.ValueInt64(), data.Servers.ValueInt64()-1),
		)
	}

	if data.Ports.IsNull() || data.Ports.IsUnknown() {
		return
	}
	var ports []portModel
	resp.Diagnostics.Append(data.Ports.ElementsAs(ctx, &ports, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validatePorts(ports)...)
}

// validatePorts rejects two host ports sharing a container port. The load
// balancer keys its config on the container port, so removing one mapping would
// break the other.
func validatePorts(ports []portModel) diag.Diagnostics {
	var diags diag.Diagnostics
	seen := map[string]bool{}
	for _, p := range ports {
		if p.ContainerPort.IsUnknown() || p.Protocol.IsUnknown() {
			continue
		}
		key := fmt.Sprintf("%d/%s", p.ContainerPort.ValueInt64(), protocolOrDefault(p.Protocol))
		if seen[key] {
			diags.AddAttributeError(path.Root("ports"), "Duplicate container port", fmt.Sprintf("container port %s is mapped more than once.", key))
		}
		seen[key] = true
	}
	return diags
}

func protocolOrDefault(protocol types.String) string {
	if protocol.IsNull() {
		return "tcp"
	}
	return protocol.ValueString()
}

// ModifyPlan forces replacement when a create-only attribute changes.
func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}

	var state, plan clusterModel
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
		{"servers", state.Servers, plan.Servers},
		{"agents", state.Agents, plan.Agents},
		{"api", state.API, plan.API},
		{"network", state.Network, plan.Network},
		{"subnet", state.Subnet, plan.Subnet},
		{"token", state.Token, plan.Token},
		{"traefik", state.Traefik, plan.Traefik},
		{"volumes", state.Volumes, plan.Volumes},
		{"k3s_args", state.K3sArgs, plan.K3sArgs},
		{"node_labels", state.NodeLabels, plan.NodeLabels},
		{"runtime_labels", state.RuntimeLabels, plan.RuntimeLabels},
		{"env", state.Env, plan.Env},
		{"host_aliases", state.HostAliases, plan.HostAliases},
		{"registries", state.Registries, plan.Registries},
	}, imported)...)
}

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec, diags := clusterSpec(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	createCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	state, err := k3dclient.CreateCluster(createCtx, spec)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create cluster", err.Error())
		return
	}

	resp.Diagnostics.Append(applyClusterState(ctx, &plan, state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := k3dclient.GetCluster(ctx, data.ID.ValueString())
	if errors.Is(err, k3dclient.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster", err.Error())
		return
	}

	resp.Diagnostics.Append(applyClusterState(ctx, &data, state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// Update applies port changes against the live cluster, so drift is corrected
// too. Everything else that reaches here is an in-place change to state only.
func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	desired, diags := portsFromModel(ctx, plan.Ports)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	current, err := k3dclient.GetCluster(ctx, plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster", err.Error())
		return
	}

	remove, add := k3dclient.DiffPorts(current.Ports, desired)
	if err := k3dclient.UpdatePorts(ctx, plan.ID.ValueString(), remove, add); err != nil {
		resp.Diagnostics.AddError("Failed to update ports", err.Error())
		return
	}

	state, err := k3dclient.GetCluster(ctx, plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read cluster", err.Error())
		return
	}

	resp.Diagnostics.Append(applyClusterState(ctx, &plan, state)...)
	resp.Diagnostics.Append(clearImported(ctx, resp.Private)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := k3dclient.DeleteCluster(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to delete cluster", err.Error())
	}
}

// ImportState imports a cluster by name. Attributes k3d cannot report stay null
// and are adopted from configuration on the next apply instead of forcing a
// replacement.
func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(markImported(ctx, resp.Private)...)
}

// applyClusterState copies everything readable from the cluster onto the model
// and leaves create-only attributes as they are.
func applyClusterState(ctx context.Context, data *clusterModel, state *k3dclient.ClusterState) diag.Diagnostics {
	var diags diag.Diagnostics

	data.ID = types.StringValue(state.Name)
	data.Name = types.StringValue(state.Name)
	data.Image = types.StringValue(state.Image)
	data.Servers = types.Int64Value(int64(state.Servers))
	data.Agents = types.Int64Value(int64(state.Agents))
	data.Network = types.StringValue(state.Network)
	data.Token = types.StringValue(state.Token)
	data.Kubeconfig = types.StringValue(state.Kubeconfig.Raw)
	data.Host = types.StringValue(state.Kubeconfig.Host)
	data.ClientCertificate = types.StringValue(state.Kubeconfig.ClientCertificate)
	data.ClientKey = types.StringValue(state.Kubeconfig.ClientKey)
	data.ClusterCACertificate = types.StringValue(state.Kubeconfig.ClusterCACertificate)

	api, d := types.ObjectValueFrom(ctx, apiAttrTypes, apiModel{
		Host:   types.StringValue(state.APIHost),
		HostIP: types.StringValue(state.APIHostIP),
		Port:   types.Int64Value(int64(state.APIPort)),
	})
	diags.Append(d...)
	data.API = api

	ports, d := portsToSet(ctx, state.Ports, data.Ports)
	diags.Append(d...)
	data.Ports = ports

	return diags
}

// portsToSet builds the ports set from observed ports. Ports already in prior
// keep their prior form, so writing protocol = "tcp" and leaving it unset both
// stay free of diffs.
func portsToSet(ctx context.Context, observed []k3dclient.Port, prior types.Set) (types.Set, diag.Diagnostics) {
	var diags diag.Diagnostics
	elemType := types.ObjectType{AttrTypes: portAttrTypes}

	if len(observed) == 0 && (prior.IsNull() || prior.IsUnknown()) {
		return types.SetNull(elemType), diags
	}

	byPort := map[k3dclient.Port]portModel{}
	if !prior.IsNull() && !prior.IsUnknown() {
		var existing []portModel
		diags.Append(prior.ElementsAs(ctx, &existing, false)...)
		for _, m := range existing {
			byPort[portFromModel(m).Normalized()] = m
		}
	}

	models := make([]portModel, 0, len(observed))
	for _, p := range observed {
		n := p.Normalized()
		if m, ok := byPort[n]; ok {
			models = append(models, m)
			continue
		}
		models = append(models, portModelFrom(n))
	}

	set, d := types.SetValueFrom(ctx, elemType, models)
	diags.Append(d...)
	return set, diags
}

func portFromModel(m portModel) k3dclient.Port {
	return k3dclient.Port{
		HostIP:        m.HostIP.ValueString(),
		HostPort:      int(m.HostPort.ValueInt64()),
		ContainerPort: int(m.ContainerPort.ValueInt64()),
		Protocol:      m.Protocol.ValueString(),
	}
}

func portModelFrom(p k3dclient.Port) portModel {
	n := p.Normalized()
	m := portModel{
		HostIP:        types.StringNull(),
		HostPort:      types.Int64Value(int64(n.HostPort)),
		ContainerPort: types.Int64Value(int64(n.ContainerPort)),
		Protocol:      types.StringNull(),
	}
	if n.HostIP != "" {
		m.HostIP = types.StringValue(n.HostIP)
	}
	if n.Protocol != "tcp" {
		m.Protocol = types.StringValue(n.Protocol)
	}
	return m
}

func portsFromModel(ctx context.Context, set types.Set) ([]k3dclient.Port, diag.Diagnostics) {
	var diags diag.Diagnostics
	if set.IsNull() || set.IsUnknown() {
		return nil, diags
	}
	var models []portModel
	diags.Append(set.ElementsAs(ctx, &models, false)...)
	ports := make([]k3dclient.Port, 0, len(models))
	for _, m := range models {
		ports = append(ports, portFromModel(m))
	}
	return ports, diags
}

func stringList(ctx context.Context, list types.List) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if list.IsNull() || list.IsUnknown() {
		return nil, diags
	}
	var out []string
	diags.Append(list.ElementsAs(ctx, &out, false)...)
	return out, diags
}

// clusterSpec converts a fully known plan into a k3d spec.
func clusterSpec(ctx context.Context, m clusterModel) (k3dclient.ClusterSpec, diag.Diagnostics) {
	var diags diag.Diagnostics

	timeout, err := time.ParseDuration(m.Timeout.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
	}

	spec := k3dclient.ClusterSpec{
		Name:    m.Name.ValueString(),
		Image:   m.Image.ValueString(),
		Servers: int(m.Servers.ValueInt64()),
		Agents:  int(m.Agents.ValueInt64()),
		Network: m.Network.ValueString(),
		Subnet:  m.Subnet.ValueString(),
		Token:   m.Token.ValueString(),
		Traefik: m.Traefik.ValueBool(),
		Timeout: timeout,
	}

	if !m.API.IsNull() && !m.API.IsUnknown() {
		var api apiModel
		diags.Append(m.API.As(ctx, &api, objectAsOptions())...)
		spec.APIHost = api.Host.ValueString()
		spec.APIHostIP = api.HostIP.ValueString()
		spec.APIPort = int(api.Port.ValueInt64())
	}

	ports, d := portsFromModel(ctx, m.Ports)
	diags.Append(d...)
	spec.Ports = ports

	diags.Append(specVolumes(ctx, m, &spec)...)
	diags.Append(specK3sArgs(ctx, m, &spec)...)
	diags.Append(specLabels(ctx, m, &spec)...)
	diags.Append(specEnv(ctx, m, &spec)...)
	diags.Append(specHostAliases(ctx, m, &spec)...)
	diags.Append(specRegistries(ctx, m, &spec)...)

	return spec, diags
}

func specVolumes(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.Volumes.IsNull() {
		return diags
	}
	var models []volumeModel
	diags.Append(m.Volumes.ElementsAs(ctx, &models, false)...)
	for _, v := range models {
		filters, d := stringList(ctx, v.NodeFilters)
		diags.Append(d...)
		spec.Volumes = append(spec.Volumes, k3dclient.Volume{
			Source:      v.Source.ValueString(),
			Destination: v.Destination.ValueString(),
			NodeFilters: filters,
		})
	}
	return diags
}

func specK3sArgs(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.K3sArgs.IsNull() {
		return diags
	}
	var models []k3sArgModel
	diags.Append(m.K3sArgs.ElementsAs(ctx, &models, false)...)
	for _, a := range models {
		filters, d := stringList(ctx, a.NodeFilters)
		diags.Append(d...)
		spec.K3sArgs = append(spec.K3sArgs, k3dclient.K3sArg{Arg: a.Arg.ValueString(), NodeFilters: filters})
	}
	return diags
}

func specLabels(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	convert := func(set types.Set) []k3dclient.Label {
		if set.IsNull() {
			return nil
		}
		var models []labelModel
		diags.Append(set.ElementsAs(ctx, &models, false)...)
		labels := make([]k3dclient.Label, 0, len(models))
		for _, l := range models {
			filters, d := stringList(ctx, l.NodeFilters)
			diags.Append(d...)
			labels = append(labels, k3dclient.Label{Key: l.Key.ValueString(), Value: l.Value.ValueString(), NodeFilters: filters})
		}
		return labels
	}
	spec.NodeLabels = convert(m.NodeLabels)
	spec.RuntimeLabels = convert(m.RuntimeLabels)
	return diags
}

func specEnv(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.Env.IsNull() {
		return diags
	}
	var models []envModel
	diags.Append(m.Env.ElementsAs(ctx, &models, false)...)
	for _, e := range models {
		filters, d := stringList(ctx, e.NodeFilters)
		diags.Append(d...)
		spec.Env = append(spec.Env, k3dclient.EnvVar{Name: e.Name.ValueString(), Value: e.Value.ValueString(), NodeFilters: filters})
	}
	return diags
}

func specHostAliases(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.HostAliases.IsNull() {
		return diags
	}
	var models []hostAliasModel
	diags.Append(m.HostAliases.ElementsAs(ctx, &models, false)...)
	for _, a := range models {
		hostnames, d := stringList(ctx, a.Hostnames)
		diags.Append(d...)
		spec.HostAliases = append(spec.HostAliases, k3dclient.HostAlias{IP: a.IP.ValueString(), Hostnames: hostnames})
	}
	return diags
}

func specRegistries(ctx context.Context, m clusterModel, spec *k3dclient.ClusterSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.Registries.IsNull() {
		return diags
	}
	var registries registriesModel
	diags.Append(m.Registries.As(ctx, &registries, objectAsOptions())...)
	use, d := stringList(ctx, registries.Use)
	diags.Append(d...)
	spec.RegistriesUse = use
	spec.RegistriesConfig = registries.Config.ValueString()
	return diags
}
