package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
)

const manifestImportSeparator = "|"

var (
	_ resource.Resource                     = (*manifestResource)(nil)
	_ resource.ResourceWithImportState      = (*manifestResource)(nil)
	_ resource.ResourceWithConfigValidators = (*manifestResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*manifestResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*manifestResource)(nil)
)

// NewManifestResource returns the k3d_manifest resource.
func NewManifestResource() resource.Resource {
	return &manifestResource{}
}

type manifestResource struct{}

type manifestModel struct {
	ID         types.String `tfsdk:"id"`
	Cluster    types.String `tfsdk:"cluster"`
	Kubeconfig types.String `tfsdk:"kubeconfig"`
	Manifest   types.String `tfsdk:"manifest"`
	Wait       types.Bool   `tfsdk:"wait"`
	Timeout    types.String `tfsdk:"timeout"`
	APIVersion types.String `tfsdk:"api_version"`
	Kind       types.String `tfsdk:"kind"`
	Namespace  types.String `tfsdk:"namespace"`
	Name       types.String `tfsdk:"name"`
}

func (m manifestModel) ref() kube.Ref {
	return kube.Ref{
		APIVersion: m.APIVersion.ValueString(),
		Kind:       m.Kind.ValueString(),
		Namespace:  m.Namespace.ValueString(),
		Name:       m.Name.ValueString(),
	}
}

func (r *manifestResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_manifest"
}

func (r *manifestResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	computedIdentity := func(description string) schema.StringAttribute {
		return schema.StringAttribute{MarkdownDescription: description, Computed: true}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Any single Kubernetes object, built in or custom, applied with server-side apply. " +
			"The content is not checked against the cluster during plan, so a custom resource can share an apply with its CRD, and a mistake in it fails at apply. " +
			"Refresh only checks that the object exists, so changes made to its contents outside Terraform are not detected. " +
			"Changing the kind, name, or namespace replaces the object. Creating one that already exists fails: import it instead. " +
			"For multiple objects, use `for_each`. Import with `<cluster>|<apiVersion>|<kind>|<namespace>|<name>`, leaving the namespace empty for cluster-scoped objects.",
		Attributes: map[string]schema.Attribute{
			"id":         computedString("UID of the object.", false),
			"cluster":    clusterAttribute(),
			"kubeconfig": kubeconfigAttribute(),
			"manifest": schema.StringAttribute{
				MarkdownDescription: "YAML or JSON holding exactly one object. Values in it appear in plans, so use `k3d_secret` for secrets.",
				Required:            true,
				Validators:          []validator.String{validManifest()},
			},
			"wait": schema.BoolAttribute{
				MarkdownDescription: "Wait for the object to be ready, such as a Deployment finishing its rollout. Defaults to `false`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "How long to wait. Defaults to `5m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultWaitTimeout),
				Validators:          []validator.String{duration()},
			},
			"api_version": computedIdentity("API version of the object."),
			"kind":        computedIdentity("Kind of the object."),
			"namespace":   computedIdentity("Namespace of the object, null if cluster-scoped."),
			"name":        computedIdentity("Name of the object."),
		},
	}
}

func validManifest() validator.String {
	return stringCheck{
		description: "must hold exactly one Kubernetes object",
		check: func(manifest string) error {
			_, err := kube.ParseManifest(manifest)
			return err
		},
	}
}

func (r *manifestResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

// ValidateConfig warns about secrets, whose values would appear in plans.
func (r *manifestResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data manifestModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || data.Manifest.IsNull() || data.Manifest.IsUnknown() {
		return
	}
	if obj, err := kube.ParseManifest(data.Manifest.ValueString()); err == nil && obj.GetKind() == "Secret" {
		resp.Diagnostics.AddAttributeWarning(path.Root("manifest"), "Secret in a manifest",
			"Values in manifest appear in plans and state. Use k3d_secret, which can keep them out.")
	}
}

// ModifyPlan parses the manifest without contacting the cluster. It fills in the
// object's identity and replaces the object if that identity changes.
func (r *manifestResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan manifestModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	hasState := !req.State.Raw.IsNull()
	var state manifestModel
	if hasState {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if plan.Manifest.IsUnknown() {
		return
	}
	obj, err := kube.ParseManifest(plan.Manifest.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("manifest"), "Invalid manifest", err.Error())
		return
	}

	namespace := types.StringUnknown()
	switch {
	case obj.GetNamespace() != "":
		namespace = types.StringValue(obj.GetNamespace())
	case hasState:
		namespace = state.Namespace
	}

	if hasState && identityChanged(state, obj) {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("manifest"))
		if obj.GetNamespace() == "" {
			namespace = types.StringUnknown()
		}
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("api_version"), obj.GetAPIVersion())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("kind"), obj.GetKind())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("name"), obj.GetName())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("namespace"), namespace)...)
}

// identityChanged compares the group, not the version, since a new version of
// the same API refers to the same object.
func identityChanged(state manifestModel, obj *unstructured.Unstructured) bool {
	if groupOf(state.APIVersion.ValueString()) != groupOf(obj.GetAPIVersion()) {
		return true
	}
	if state.Kind.ValueString() != obj.GetKind() || state.Name.ValueString() != obj.GetName() {
		return true
	}
	return obj.GetNamespace() != "" && obj.GetNamespace() != state.Namespace.ValueString()
}

func groupOf(apiVersion string) string {
	gv, err := k8sschema.ParseGroupVersion(apiVersion)
	if err != nil {
		return apiVersion
	}
	return gv.Group
}

// applyManifest applies the plan and records what the API returned.
func applyManifest(ctx context.Context, client *kube.Client, plan *manifestModel) (*unstructured.Unstructured, error) {
	obj, err := kube.ParseManifest(plan.Manifest.ValueString())
	if err != nil {
		return nil, err
	}
	applied, err := client.Apply(ctx, obj)
	if err != nil {
		return nil, err
	}
	setManifestIdentity(plan, applied)
	return applied, nil
}

func setManifestIdentity(m *manifestModel, obj *unstructured.Unstructured) {
	m.ID = types.StringValue(string(obj.GetUID()))
	m.APIVersion = types.StringValue(obj.GetAPIVersion())
	m.Kind = types.StringValue(obj.GetKind())
	m.Name = types.StringValue(obj.GetName())
	m.Namespace = types.StringNull()
	if obj.GetNamespace() != "" {
		m.Namespace = types.StringValue(obj.GetNamespace())
	}
}

func (r *manifestResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan manifestModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	timeout, err := parseTimeout(plan.Timeout, defaultWaitTimeout)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
		return
	}
	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	obj, err := kube.ParseManifest(plan.Manifest.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("manifest"), "Invalid manifest", err.Error())
		return
	}
	exists, err := client.Exists(ctx, kube.RefOf(obj))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the object", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("manifest"), "Object already exists",
			fmt.Sprintf("%s already exists: import it so that it has one owner.", kube.RefOf(obj)))
		return
	}

	applied, err := applyManifest(ctx, client, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to apply the manifest", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() || !plan.Wait.ValueBool() {
		return
	}
	if err := client.WaitReady(ctx, kube.RefOf(applied), timeout); err != nil {
		resp.Diagnostics.AddError("Object did not become ready", err.Error())
	}
}

func (r *manifestResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data manifestModel
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

	obj, err := client.Get(ctx, data.ref())
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the object", err.Error())
		return
	}

	data.ID = types.StringValue(string(obj.GetUID()))
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func (r *manifestResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan manifestModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	timeout, err := parseTimeout(plan.Timeout, defaultWaitTimeout)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
		return
	}
	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	applied, err := applyManifest(ctx, client, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to apply the manifest", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() || !plan.Wait.ValueBool() {
		return
	}
	if err := client.WaitReady(ctx, kube.RefOf(applied), timeout); err != nil {
		resp.Diagnostics.AddError("Object did not become ready", err.Error())
	}
}

func (r *manifestResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state manifestModel
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
	if err := client.Delete(ctx, state.ref(), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the object", err.Error())
	}
}

// ImportState imports with <cluster>|<apiVersion>|<kind>|<namespace>|<name>. The
// separator is one that appears in neither names nor API versions such as
// networking.istio.io/v1. The next apply replaces the manifest with your own.
func (r *manifestResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, manifestImportSeparator)
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[4] == "" {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("invalid import ID %q: want <cluster>|<apiVersion>|<kind>|<namespace>|<name>", req.ID))
		return
	}

	obj := newObject(parts[1], parts[2], parts[3], parts[4])
	encoded, err := obj.MarshalJSON()
	if err != nil {
		resp.Diagnostics.AddError("Failed to build the manifest", err.Error())
		return
	}

	namespace := types.StringNull()
	if parts[3] != "" {
		namespace = types.StringValue(parts[3])
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("manifest"), string(encoded))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("api_version"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("kind"), parts[2])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), namespace)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), parts[4])...)
}
