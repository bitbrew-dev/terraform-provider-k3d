package provider

import (
	"context"
	"fmt"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func dataConnectionAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"cluster": schema.StringAttribute{
			MarkdownDescription: "Cluster to read from. Exactly one of `cluster` or `kubeconfig` is required.",
			Optional:            true,
		},
		"kubeconfig": schema.StringAttribute{
			MarkdownDescription: "Kubeconfig contents, for a cluster reached directly and not through Docker. It is stored in Terraform state.",
			Optional:            true,
			Sensitive:           true,
		},
	}
}

func dataConnectionValidators() []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("cluster"), path.MatchRoot("kubeconfig")),
	}
}

func withAttributes(base map[string]schema.Attribute, extra map[string]schema.Attribute) map[string]schema.Attribute {
	for name, attribute := range extra {
		base[name] = attribute
	}
	return base
}

func computedMapAttribute(description string, sensitive bool) schema.MapAttribute {
	return schema.MapAttribute{MarkdownDescription: description, ElementType: types.StringType, Computed: true, Sensitive: sensitive}
}

func optionalNamespace() schema.StringAttribute {
	return schema.StringAttribute{MarkdownDescription: "Namespace of the object. Defaults to `default`.", Optional: true, Computed: true}
}

func namespaceOrDefault(namespace types.String) string {
	if namespace.IsNull() || namespace.IsUnknown() || namespace.ValueString() == "" {
		return defaultNamespace
	}
	return namespace.ValueString()
}

// readObject fetches an object for a data source. A missing object or cluster
// is an error here, unlike in a resource, because nothing can be read.
func readObject(ctx context.Context, cluster, kubeconfig types.String, ref kube.Ref) (*unstructured.Unstructured, diag.Diagnostics) {
	var diags diag.Diagnostics
	client, err := connect(ctx, cluster, kubeconfig)
	if err != nil {
		diags.AddError("Failed to connect to the cluster", err.Error())
		return nil, diags
	}
	obj, err := client.Get(ctx, ref)
	if err != nil {
		diags.AddError("Failed to read "+ref.String(), err.Error())
		return nil, diags
	}
	return obj, diags
}

func stringMapValue(ctx context.Context, values map[string]string) (types.Map, diag.Diagnostics) {
	if values == nil {
		values = map[string]string{}
	}
	return types.MapValueFrom(ctx, types.StringType, values)
}

// namespace data source

var _ datasource.DataSourceWithConfigValidators = (*namespaceDataSource)(nil)

// NewNamespaceDataSource returns the k3d_namespace data source.
func NewNamespaceDataSource() datasource.DataSource { return &namespaceDataSource{} }

type namespaceDataSource struct{}

type namespaceDataModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	Labels      types.Map    `tfsdk:"labels"`
	Annotations types.Map    `tfsdk:"annotations"`
	Phase       types.String `tfsdk:"phase"`
}

func (d *namespaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_namespace"
}

func (d *namespaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing namespace.",
		Attributes: withAttributes(dataConnectionAttributes(), map[string]schema.Attribute{
			"id":          schema.StringAttribute{MarkdownDescription: "UID of the namespace.", Computed: true},
			"name":        schema.StringAttribute{MarkdownDescription: "Namespace name.", Required: true},
			"labels":      computedMapAttribute("All labels.", false),
			"annotations": computedMapAttribute("All annotations.", false),
			"phase":       schema.StringAttribute{MarkdownDescription: "`Active` or `Terminating`.", Computed: true},
		}),
	}
}

func (d *namespaceDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return dataConnectionValidators()
}

func (d *namespaceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data namespaceDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj, diags := readObject(ctx, data.Cluster, data.Kubeconfig, namespaceRef(data.Name.ValueString()))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.ID = types.StringValue(string(obj.GetUID()))
	labels, diags := stringMapValue(ctx, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := stringMapValue(ctx, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	data.Labels, data.Annotations = labels, annotations
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	data.Phase = types.StringValue(phase)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// config map data source

var _ datasource.DataSourceWithConfigValidators = (*configMapDataSource)(nil)

// NewConfigMapDataSource returns the k3d_config_map data source.
func NewConfigMapDataSource() datasource.DataSource { return &configMapDataSource{} }

type configMapDataSource struct{}

type configMapDataModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	Namespace   types.String `tfsdk:"namespace"`
	Labels      types.Map    `tfsdk:"labels"`
	Annotations types.Map    `tfsdk:"annotations"`
	Data        types.Map    `tfsdk:"data"`
}

func (d *configMapDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config_map"
}

func (d *configMapDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing ConfigMap.",
		Attributes: withAttributes(dataConnectionAttributes(), map[string]schema.Attribute{
			"id":          schema.StringAttribute{MarkdownDescription: "UID of the ConfigMap.", Computed: true},
			"name":        schema.StringAttribute{MarkdownDescription: "ConfigMap name.", Required: true},
			"namespace":   optionalNamespace(),
			"labels":      computedMapAttribute("All labels.", false),
			"annotations": computedMapAttribute("All annotations.", false),
			"data":        computedMapAttribute("Keys and values.", false),
		}),
	}
}

func (d *configMapDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return dataConnectionValidators()
}

func (d *configMapDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data configMapDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	namespace := namespaceOrDefault(data.Namespace)
	obj, diags := readObject(ctx, data.Cluster, data.Kubeconfig, configMapRef(namespace, data.Name.ValueString()))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	live, _, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the config map data", err.Error())
		return
	}

	data.ID = types.StringValue(string(obj.GetUID()))
	data.Namespace = types.StringValue(namespace)
	labels, diags := stringMapValue(ctx, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := stringMapValue(ctx, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	values, diags := stringMapValue(ctx, live)
	resp.Diagnostics.Append(diags...)
	data.Labels, data.Annotations, data.Data = labels, annotations, values
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// secret data source

var _ datasource.DataSourceWithConfigValidators = (*secretDataSource)(nil)

// NewSecretDataSource returns the k3d_secret data source.
func NewSecretDataSource() datasource.DataSource { return &secretDataSource{} }

type secretDataSource struct{}

type secretDataModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	Namespace   types.String `tfsdk:"namespace"`
	Labels      types.Map    `tfsdk:"labels"`
	Annotations types.Map    `tfsdk:"annotations"`
	Type        types.String `tfsdk:"type"`
	Data        types.Map    `tfsdk:"data"`
}

func (d *secretDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (d *secretDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing Secret. Its values are stored in Terraform state, so use an encrypted state backend.",
		Attributes: withAttributes(dataConnectionAttributes(), map[string]schema.Attribute{
			"id":          schema.StringAttribute{MarkdownDescription: "UID of the Secret.", Computed: true},
			"name":        schema.StringAttribute{MarkdownDescription: "Secret name.", Required: true},
			"namespace":   optionalNamespace(),
			"labels":      computedMapAttribute("All labels.", false),
			"annotations": computedMapAttribute("All annotations.", false),
			"type":        schema.StringAttribute{MarkdownDescription: "Secret type.", Computed: true},
			"data":        computedMapAttribute("Keys and decoded values.", true),
		}),
	}
}

func (d *secretDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return dataConnectionValidators()
}

func (d *secretDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data secretDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	namespace := namespaceOrDefault(data.Namespace)
	obj, diags := readObject(ctx, data.Cluster, data.Kubeconfig, secretRef(namespace, data.Name.ValueString()))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	live, err := decodeSecretData(obj)
	if err != nil {
		resp.Diagnostics.AddError("Failed to decode the secret", err.Error())
		return
	}
	secretType, _, _ := unstructured.NestedString(obj.Object, "type")

	data.ID = types.StringValue(string(obj.GetUID()))
	data.Namespace = types.StringValue(namespace)
	data.Type = types.StringValue(secretType)
	labels, diags := stringMapValue(ctx, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := stringMapValue(ctx, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	values, diags := stringMapValue(ctx, live)
	resp.Diagnostics.Append(diags...)
	data.Labels, data.Annotations, data.Data = labels, annotations, values
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// context data source

var _ datasource.DataSourceWithConfigValidators = (*contextDataSource)(nil)

// NewContextDataSource returns the k3d_context data source.
func NewContextDataSource() datasource.DataSource { return &contextDataSource{} }

type contextDataSource struct{}

type contextDataModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	ClusterName types.String `tfsdk:"cluster_name"`
	UserName    types.String `tfsdk:"user_name"`
	Namespace   types.String `tfsdk:"namespace"`
	Host        types.String `tfsdk:"host"`
}

func (d *contextDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_context"
}

func (d *contextDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{MarkdownDescription: description, Computed: true}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the kubeconfig context of a k3d cluster, or the current context of a kubeconfig, without contacting the Kubernetes API. Use `k3d_cluster` to also get credentials.",
		Attributes: withAttributes(dataConnectionAttributes(), map[string]schema.Attribute{
			"id":           computed("The context name."),
			"name":         computed("Context name, such as `k3d-demo`."),
			"cluster_name": computed("Name of the context's cluster entry."),
			"user_name":    computed("Name of the context's user entry."),
			"namespace":    computed("Default namespace of the context, if any."),
			"host":         computed("Kubernetes API URL."),
		}),
	}
}

func (d *contextDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return dataConnectionValidators()
}

func (d *contextDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data contextDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	raw, err := resolveKubeconfig(ctx, data.Cluster, data.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the kubeconfig", err.Error())
		return
	}
	current, err := kube.CurrentContext(raw)
	if err != nil {
		resp.Diagnostics.AddError("Invalid kubeconfig", fmt.Sprintf("read current context: %s", err))
		return
	}

	data.ID = types.StringValue(current.Name)
	data.Name = types.StringValue(current.Name)
	data.ClusterName = types.StringValue(current.ClusterName)
	data.UserName = types.StringValue(current.UserName)
	data.Namespace = types.StringValue(current.Namespace)
	data.Host = types.StringValue(current.Host)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}
