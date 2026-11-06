package provider

import (
	"context"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var (
	_ resource.Resource                     = (*configMapResource)(nil)
	_ resource.ResourceWithImportState      = (*configMapResource)(nil)
	_ resource.ResourceWithConfigValidators = (*configMapResource)(nil)
)

// NewConfigMapResource returns the k3d_config_map resource.
func NewConfigMapResource() resource.Resource {
	return &configMapResource{}
}

type configMapResource struct{}

type configMapModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	Namespace   types.String `tfsdk:"namespace"`
	Labels      types.Map    `tfsdk:"labels"`
	Annotations types.Map    `tfsdk:"annotations"`
	Data        types.Map    `tfsdk:"data"`
}

func configMapRef(namespace, name string) kube.Ref {
	return kube.Ref{APIVersion: "v1", Kind: "ConfigMap", Namespace: namespace, Name: name}
}

func (r *configMapResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config_map"
}

func (r *configMapResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Kubernetes ConfigMap. Creating one that already exists fails: import it instead. Import with `<cluster>/<namespace>/<name>`.",
		Attributes: map[string]schema.Attribute{
			"id":          computedString("`<cluster>/<namespace>/<name>`.", false),
			"cluster":     clusterAttribute(),
			"kubeconfig":  kubeconfigAttribute(),
			"name":        requiredReplaceString("ConfigMap name."),
			"namespace":   namespaceAttribute(),
			"labels":      metadataMapAttribute("Labels."),
			"annotations": metadataMapAttribute("Annotations."),
			"data": schema.MapAttribute{
				MarkdownDescription: "Keys and values. Only keys set here are tracked.",
				ElementType:         types.StringType,
				Optional:            true,
			},
		},
	}
}

func (r *configMapResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

func buildConfigMap(ctx context.Context, m configMapModel) (*unstructured.Unstructured, error) {
	obj := newObject("v1", "ConfigMap", m.Namespace.ValueString(), m.Name.ValueString())
	if diags := setMetadata(ctx, obj, m.Labels, m.Annotations); diags.HasError() {
		return nil, diagsError(diags)
	}
	data, diags := stringMap(ctx, m.Data)
	if diags.HasError() {
		return nil, diagsError(diags)
	}
	if len(data) > 0 {
		values := make(map[string]any, len(data))
		for k, v := range data {
			values[k] = v
		}
		if err := unstructured.SetNestedMap(obj.Object, values, "data"); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

func (r *configMapResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan configMapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	namespace, name := plan.Namespace.ValueString(), plan.Name.ValueString()
	exists, err := client.Exists(ctx, configMapRef(namespace, name))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the config map", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "ConfigMap already exists",
			"ConfigMap "+namespace+"/"+name+" already exists: import it so that it has one owner.")
		return
	}

	if err := applyConfigMap(ctx, client, plan); err != nil {
		resp.Diagnostics.AddError("Failed to apply the config map", err.Error())
		return
	}
	plan.ID = types.StringValue(objectID(plan.Cluster, namespace, name))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func applyConfigMap(ctx context.Context, client *kube.Client, m configMapModel) error {
	obj, err := buildConfigMap(ctx, m)
	if err != nil {
		return err
	}
	_, err = client.Apply(ctx, obj)
	return err
}

func (r *configMapResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data configMapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, data.Cluster, data.Kubeconfig)
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	obj, err := client.Get(ctx, configMapRef(data.Namespace.ValueString(), data.Name.ValueString()))
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the config map", err.Error())
		return
	}

	live, _, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the config map data", err.Error())
		return
	}

	labels, diags := trackedMap(ctx, data.Labels, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := trackedMap(ctx, data.Annotations, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	values, diags := trackedMap(ctx, data.Data, live)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.Labels = labels
	data.Annotations = annotations
	data.Data = values
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func (r *configMapResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan configMapModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}
	if err := applyConfigMap(ctx, client, plan); err != nil {
		resp.Diagnostics.AddError("Failed to apply the config map", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *configMapResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state configMapModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, state.Cluster, state.Kubeconfig)
	if isGone(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}
	if err := client.Delete(ctx, configMapRef(state.Namespace.ValueString(), state.Name.ValueString()), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the config map", err.Error())
	}
}

func (r *configMapResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importNamespaced(ctx, req, resp)
}

// importNamespaced imports an object with the ID <cluster>/<namespace>/<name>.
func importNamespaced(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, err := splitImportID(req.ID, "<cluster>/<namespace>/<name>", 3)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[2])...)
}
