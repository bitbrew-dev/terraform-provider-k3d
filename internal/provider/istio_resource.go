package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	defaultIstioRepository  = "https://istio-release.storage.googleapis.com/charts"
	defaultIstioNamespace   = "istio-system"
	defaultGatewayName      = "istio-ingressgateway"
	defaultGatewayService   = "NodePort"
	istioBaseRelease        = "istio-base"
	istiodRelease           = "istiod"
	istioMissing            = "missing"
	appliedIstioRevisionKey = "applied_revisions"
)

var istioVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+`)

type istioResource struct{}

type istioModel struct {
	ID              types.String `tfsdk:"id"`
	Cluster         types.String `tfsdk:"cluster"`
	Kubeconfig      types.String `tfsdk:"kubeconfig"`
	Version         types.String `tfsdk:"version"`
	Namespace       types.String `tfsdk:"namespace"`
	Repository      types.String `tfsdk:"repository"`
	BaseValues      types.List   `tfsdk:"base_values"`
	IstiodValues    types.List   `tfsdk:"istiod_values"`
	Gateway         types.Object `tfsdk:"gateway"`
	TakeOwnership   types.Bool   `tfsdk:"take_ownership"`
	Timeout         types.String `tfsdk:"timeout"`
	BaseRevision    types.Int64  `tfsdk:"base_revision"`
	IstiodRevision  types.Int64  `tfsdk:"istiod_revision"`
	GatewayRevision types.Int64  `tfsdk:"gateway_revision"`
	Status          types.String `tfsdk:"status"`
}

type istioGatewayModel struct {
	Name          types.String `tfsdk:"name"`
	ServiceType   types.String `tfsdk:"service_type"`
	HTTPNodePort  types.Int64  `tfsdk:"http_node_port"`
	HTTPSNodePort types.Int64  `tfsdk:"https_node_port"`
	Values        types.List   `tfsdk:"values"`
}

var istioGatewayAttrTypes = map[string]attr.Type{
	"name":            types.StringType,
	"service_type":    types.StringType,
	"http_node_port":  types.Int64Type,
	"https_node_port": types.Int64Type,
	"values":          types.ListType{ElemType: types.StringType},
}

func (r *istioResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_istio"
}

func (r *istioResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	revision := func(description string) schema.Int64Attribute {
		return schema.Int64Attribute{MarkdownDescription: description, Computed: true}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Istio installed from its official Helm charts: `base`, then `istiod`, then an optional `gateway`, all at one pinned `version`. " +
			"Create the namespace first, for example with `k3d_namespace`, and make workloads that use sidecar injection depend on this resource. " +
			"With a gateway, do not label the namespace `istio-injection: disabled`: the gateway gets its proxy image through injection, so its pod never starts. " +
			"A component that is removed or changed outside Terraform is restored by the next apply. " +
			"Destroying this resource leaves Istio's CRDs in the cluster, as the chart intends, and reinstalling reuses them. " +
			"An install made with `istioctl` has no Helm releases, so it cannot be imported: set `take_ownership` to adopt its objects with a new install. " +
			"Import a Helm install with `<cluster>/<namespace>`. If Traefik is enabled on the cluster, a `LoadBalancer` gateway conflicts with it on ports 80 and 443.",
		Attributes: map[string]schema.Attribute{
			"id":         computedString("`<cluster>/<namespace>`.", false),
			"cluster":    clusterAttribute(),
			"kubeconfig": kubeconfigAttribute(),
			"version": schema.StringAttribute{
				MarkdownDescription: "Istio version, such as `1.30.1`. Changing it upgrades every component in order.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(istioVersionPattern, "must be an exact version such as 1.30.1"),
				},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Existing namespace for every component. Defaults to `istio-system`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultIstioNamespace),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"repository": schema.StringAttribute{
				MarkdownDescription: "Chart repository, for use with a mirror. Defaults to Istio's official repository.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultIstioRepository),
			},
			"base_values": schema.ListAttribute{
				MarkdownDescription: "YAML documents for the `base` chart, merged in order.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"istiod_values": schema.ListAttribute{
				MarkdownDescription: "YAML documents for the `istiod` chart, merged in order, such as `meshConfig` or `pilot.resources`.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"gateway": schema.SingleNestedAttribute{
				MarkdownDescription: "An ingress gateway. Left out, no gateway is installed.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						MarkdownDescription: "Release name, which also sets the pod labels. The default `istio-ingressgateway` gives `istio: ingressgateway`, which a `Gateway` object selects.",
						Optional:            true,
					},
					"service_type": schema.StringAttribute{
						MarkdownDescription: "Service type. Defaults to `NodePort`, which pairs with `k3d_cluster.ports`.",
						Optional:            true,
						Validators:          []validator.String{stringvalidator.OneOf("NodePort", "LoadBalancer", "ClusterIP")},
					},
					"http_node_port": schema.Int64Attribute{
						MarkdownDescription: "Fixed NodePort for port 80, so that `k3d_cluster.ports` can target it.",
						Optional:            true,
						Validators:          []validator.Int64{int64validator.Between(1, 65535)},
					},
					"https_node_port": schema.Int64Attribute{
						MarkdownDescription: "Fixed NodePort for port 443.",
						Optional:            true,
						Validators:          []validator.Int64{int64validator.Between(1, 65535)},
					},
					"values": schema.ListAttribute{
						MarkdownDescription: "YAML documents for the `gateway` chart, merged after the settings above.",
						ElementType:         types.StringType,
						Optional:            true,
					},
				},
			},
			"take_ownership": schema.BoolAttribute{
				MarkdownDescription: "Let the install adopt existing objects, such as ones from `istioctl`. Defaults to `false`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "How long each component can take. Defaults to `10m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultReleaseTimeout),
				Validators:          []validator.String{duration()},
			},
			"base_revision":    revision("Current revision of the `base` release."),
			"istiod_revision":  revision("Current revision of the `istiod` release."),
			"gateway_revision": revision("Current revision of the `gateway` release, null without a gateway."),
			"status": schema.StringAttribute{
				MarkdownDescription: "`deployed` when every component is, otherwise the status of the first one that is not.",
				Computed:            true,
			},
		},
	}
}

func (r *istioResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

// ModifyPlan needs no cluster. A component changed outside Terraform plans an
// upgrade that restores the configuration.
func (r *istioResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var state istioModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	applied, diags := req.Private.GetKey(ctx, appliedIstioRevisionKey)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() || !istioDrifted(state, applied) {
		return
	}
	for _, name := range []string{"base_revision", "istiod_revision", "gateway_revision"} {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.Int64Unknown())...)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
}

// istioRevisions is what Terraform last applied, kept in private state.
type istioRevisions struct {
	Base    int64 `json:"base"`
	Istiod  int64 `json:"istiod"`
	Gateway int64 `json:"gateway"`
}

func istioDrifted(state istioModel, applied []byte) bool {
	if state.Status.ValueString() != statusDeployed {
		return true
	}
	if len(applied) == 0 {
		return false
	}
	var last istioRevisions
	if err := json.Unmarshal(applied, &last); err != nil {
		return true
	}
	return last != istioRevisions{
		Base:    state.BaseRevision.ValueInt64(),
		Istiod:  state.IstiodRevision.ValueInt64(),
		Gateway: state.GatewayRevision.ValueInt64(),
	}
}

// istioComponent is one chart to install.
type istioComponent struct {
	Release string
	Chart   string
	Values  map[string]any
}

func gatewayOf(ctx context.Context, m istioModel) (*istioGatewayModel, diag.Diagnostics) {
	if m.Gateway.IsNull() || m.Gateway.IsUnknown() {
		return nil, nil
	}
	var gateway istioGatewayModel
	diags := m.Gateway.As(ctx, &gateway, objectAsOptions())
	return &gateway, diags
}

func gatewayRelease(gateway *istioGatewayModel) string {
	if gateway == nil {
		return ""
	}
	if name := gateway.Name.ValueString(); name != "" {
		return name
	}
	return defaultGatewayName
}

// gatewayDefaults renders the typed settings as the chart's values. The chart
// passes service.ports through as written, which is what allows a nodePort.
func gatewayDefaults(gateway istioGatewayModel) map[string]any {
	port := func(name string, number int64, nodePort types.Int64) map[string]any {
		entry := map[string]any{"name": name, "port": number, "protocol": "TCP", "targetPort": number}
		if !nodePort.IsNull() {
			entry["nodePort"] = nodePort.ValueInt64()
		}
		return entry
	}
	serviceType := gateway.ServiceType.ValueString()
	if serviceType == "" {
		serviceType = defaultGatewayService
	}
	return map[string]any{
		"service": map[string]any{
			"type": serviceType,
			"ports": []any{
				port("status-port", 15021, types.Int64Null()),
				port("http2", 80, gateway.HTTPNodePort),
				port("https", 443, gateway.HTTPSNodePort),
			},
		},
	}
}

func mergeWithDefaults(ctx context.Context, defaults map[string]any, overrides types.List) (map[string]any, diag.Diagnostics) {
	documents, diags := stringList(ctx, overrides)
	if diags.HasError() {
		return nil, diags
	}
	encoded, err := json.Marshal(defaults)
	if err != nil {
		diags.AddError("Failed to render values", err.Error())
		return nil, diags
	}
	values, err := helm.MergeValues(append([]string{string(encoded)}, documents...), nil)
	if err != nil {
		diags.AddError("Invalid values", err.Error())
		return nil, diags
	}
	return values, diags
}

// istioComponents returns the charts to install, in install order.
func istioComponents(ctx context.Context, m istioModel) ([]istioComponent, diag.Diagnostics) {
	namespaced := map[string]any{"global": map[string]any{"istioNamespace": m.Namespace.ValueString()}}

	base, diags := mergeWithDefaults(ctx, namespaced, m.BaseValues)
	istiod, d := mergeWithDefaults(ctx, namespaced, m.IstiodValues)
	diags.Append(d...)
	gateway, d := gatewayOf(ctx, m)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}

	components := []istioComponent{
		{Release: istioBaseRelease, Chart: "base", Values: base},
		{Release: istiodRelease, Chart: "istiod", Values: istiod},
	}
	if gateway != nil {
		values, d := mergeWithDefaults(ctx, gatewayDefaults(*gateway), gateway.Values)
		diags.Append(d...)
		components = append(components, istioComponent{Release: gatewayRelease(gateway), Chart: "gateway", Values: values})
	}
	return components, diags
}

func istioID(m istioModel) types.String {
	return types.StringValue(fmt.Sprintf("%s/%s", m.Cluster.ValueString(), m.Namespace.ValueString()))
}
