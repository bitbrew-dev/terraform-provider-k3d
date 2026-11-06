package provider

import (
	"context"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*namespaceResource)(nil)
	_ resource.ResourceWithImportState      = (*namespaceResource)(nil)
	_ resource.ResourceWithConfigValidators = (*namespaceResource)(nil)
)

// NewNamespaceResource returns the k3d_namespace resource.
func NewNamespaceResource() resource.Resource {
	return &namespaceResource{}
}

type namespaceResource struct{}

type namespaceModel struct {
	ID          types.String `tfsdk:"id"`
	Cluster     types.String `tfsdk:"cluster"`
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	Name        types.String `tfsdk:"name"`
	Labels      types.Map    `tfsdk:"labels"`
	Annotations types.Map    `tfsdk:"annotations"`
}

func namespaceRef(name string) kube.Ref {
	return kube.Ref{APIVersion: "v1", Kind: "Namespace", Name: name}
}

func (r *namespaceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_namespace"
}

func (r *namespaceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Kubernetes namespace. Creating one that already exists fails: import it instead. " +
			"If a Helm chart also creates this namespace, let only one of them own it. Import with `<cluster>/<name>`.",
		Attributes: map[string]schema.Attribute{
			"id":          computedString("`<cluster>/<name>`.", false),
			"cluster":     clusterAttribute(),
			"kubeconfig":  kubeconfigAttribute(),
			"name":        requiredReplaceString("Namespace name."),
			"labels":      metadataMapAttribute("Labels, such as `istio-injection`."),
			"annotations": metadataMapAttribute("Annotations."),
		},
	}
}

func (r *namespaceResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

func (r *namespaceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan namespaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	exists, err := client.Exists(ctx, namespaceRef(plan.Name.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the namespace", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Namespace already exists",
			"Namespace "+plan.Name.ValueString()+" already exists: import it so that it has one owner.")
		return
	}

	if !r.apply(ctx, client, plan, resp.Diagnostics.AddError) {
		return
	}
	plan.ID = types.StringValue(objectID(plan.Cluster, plan.Name.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *namespaceResource) apply(ctx context.Context, client *kube.Client, m namespaceModel, addError func(string, string)) bool {
	obj := newObject("v1", "Namespace", "", m.Name.ValueString())
	if diags := setMetadata(ctx, obj, m.Labels, m.Annotations); diags.HasError() {
		addError("Invalid metadata", diags.Errors()[0].Detail())
		return false
	}
	if _, err := client.Apply(ctx, obj); err != nil {
		addError("Failed to apply the namespace", err.Error())
		return false
	}
	return true
}

func (r *namespaceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data namespaceModel
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

	obj, err := client.Get(ctx, namespaceRef(data.Name.ValueString()))
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the namespace", err.Error())
		return
	}

	labels, diags := trackedMap(ctx, data.Labels, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := trackedMap(ctx, data.Annotations, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.Labels = labels
	data.Annotations = annotations
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func (r *namespaceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan namespaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}
	if !r.apply(ctx, client, plan, resp.Diagnostics.AddError) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *namespaceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state namespaceModel
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
	if err := client.Delete(ctx, namespaceRef(state.Name.ValueString()), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the namespace", err.Error())
	}
}

func (r *namespaceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, err := splitImportID(req.ID, "<cluster>/<name>", 2)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[1])...)
}
