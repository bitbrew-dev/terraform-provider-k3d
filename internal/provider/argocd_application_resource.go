package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	argoAPIVersion       = "argoproj.io/v1alpha1"
	argoKind             = "Application"
	argoCascadeFinalizer = "resources-finalizer.argocd.argoproj.io"
	argoDefaultNamespace = "argocd"
	argoDefaultServer    = "https://kubernetes.default.svc"
	argoMovingRevision   = "HEAD"
	argoStatusUnknown    = "Unknown"
)

var (
	_ resource.Resource                     = (*argoApplicationResource)(nil)
	_ resource.ResourceWithImportState      = (*argoApplicationResource)(nil)
	_ resource.ResourceWithConfigValidators = (*argoApplicationResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*argoApplicationResource)(nil)
)

// NewArgoApplicationResource returns the k3d_argocd_application resource.
func NewArgoApplicationResource() resource.Resource {
	return &argoApplicationResource{}
}

type argoApplicationResource struct{}

type argoApplicationModel struct {
	ID            types.String `tfsdk:"id"`
	Cluster       types.String `tfsdk:"cluster"`
	Kubeconfig    types.String `tfsdk:"kubeconfig"`
	Name          types.String `tfsdk:"name"`
	Namespace     types.String `tfsdk:"namespace"`
	Project       types.String `tfsdk:"project"`
	Source        types.Object `tfsdk:"source"`
	Destination   types.Object `tfsdk:"destination"`
	SyncPolicy    types.Object `tfsdk:"sync_policy"`
	CascadeDelete types.Bool   `tfsdk:"cascade_delete"`
	WaitForSync   types.Bool   `tfsdk:"wait_for_sync"`
	Timeout       types.String `tfsdk:"timeout"`
	SyncStatus    types.String `tfsdk:"sync_status"`
	HealthStatus  types.String `tfsdk:"health_status"`
}

type argoSourceModel struct {
	RepoURL          types.String `tfsdk:"repo_url"`
	TargetRevision   types.String `tfsdk:"target_revision"`
	Path             types.String `tfsdk:"path"`
	Chart            types.String `tfsdk:"chart"`
	DirectoryRecurse types.Bool   `tfsdk:"directory_recurse"`
	HelmValues       types.String `tfsdk:"helm_values"`
	HelmReleaseName  types.String `tfsdk:"helm_release_name"`
}

type argoDestinationModel struct {
	Server    types.String `tfsdk:"server"`
	Namespace types.String `tfsdk:"namespace"`
}

type argoSyncPolicyModel struct {
	Automated   types.Bool `tfsdk:"automated"`
	Prune       types.Bool `tfsdk:"prune"`
	SelfHeal    types.Bool `tfsdk:"self_heal"`
	SyncOptions types.List `tfsdk:"sync_options"`
}

func (r *argoApplicationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_argocd_application"
}

func (r *argoApplicationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	optionalString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{MarkdownDescription: description, Optional: true}
	}
	optionalBool := func(description string) schema.BoolAttribute {
		return schema.BoolAttribute{MarkdownDescription: description, Optional: true}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "An Argo CD Application. Argo CD must be installed, usually with `k3d_helm_release`, and this resource should depend on it, so that it is deleted first: otherwise `cascade_delete` has no controller to finish it. " +
			"Refresh only reads whether it exists and its status, because Argo CD rewrites the spec, so changes made to it outside Terraform are not detected. " +
			"A private repository needs a `k3d_secret` with the label `argocd.argoproj.io/secret-type = \"repository\"`. " +
			"Creating one that already exists fails: import it instead, with `<cluster>/<namespace>/<name>`.",
		Attributes: map[string]schema.Attribute{
			"id":         computedString("`<cluster>/<namespace>/<name>`.", false),
			"cluster":    clusterAttribute(),
			"kubeconfig": kubeconfigAttribute(),
			"name":       requiredReplaceString("Application name."),
			"namespace": func() schema.StringAttribute {
				attribute := namespaceAttribute()
				attribute.MarkdownDescription = "Namespace Argo CD runs in. Defaults to `argocd`."
				attribute.Default = stringdefault.StaticString(argoDefaultNamespace)
				return attribute
			}(),
			"project": schema.StringAttribute{
				MarkdownDescription: "Argo CD project. Defaults to `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
			},
			"source": schema.SingleNestedAttribute{
				MarkdownDescription: "What to deploy. Set exactly one of `path` for a folder in a Git repository, or `chart` for a chart in a Helm repository.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"repo_url": schema.StringAttribute{MarkdownDescription: "Git or Helm repository URL.", Required: true},
					"target_revision": schema.StringAttribute{
						MarkdownDescription: "Commit, tag, or chart version. A pinned value keeps deployments reproducible, so `HEAD` gives a warning.",
						Required:            true,
					},
					"path":              optionalString("Folder in the Git repository."),
					"chart":             optionalString("Chart in the Helm repository."),
					"directory_recurse": optionalBool("Read manifests from subfolders of `path` too."),
					"helm_values":       optionalString("Helm values as YAML. These appear in the plan and in the Application, so reference secrets instead of including them."),
					"helm_release_name": optionalString("Helm release name. Defaults to the application name."),
				},
			},
			"destination": schema.SingleNestedAttribute{
				MarkdownDescription: "Where to deploy.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"server":    optionalString("Cluster API URL. Defaults to the cluster Argo CD runs in."),
					"namespace": schema.StringAttribute{MarkdownDescription: "Namespace to deploy into.", Required: true},
				},
			},
			"sync_policy": schema.SingleNestedAttribute{
				MarkdownDescription: "When to sync. Left out, syncing is manual.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"automated": optionalBool("Sync automatically when Git changes."),
					"prune":     optionalBool("With `automated`, delete objects removed from Git."),
					"self_heal": optionalBool("With `automated`, undo changes made in the cluster."),
					"sync_options": schema.ListAttribute{
						MarkdownDescription: "Options such as `CreateNamespace=true`.",
						ElementType:         types.StringType,
						Optional:            true,
					},
				},
			},
			"cascade_delete": schema.BoolAttribute{
				MarkdownDescription: "Delete the deployed objects along with the Application. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"wait_for_sync": schema.BoolAttribute{
				MarkdownDescription: "Wait until it is `Synced` and `Healthy`. Needs `sync_policy.automated`. Defaults to `false`.",
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
			"sync_status":   schema.StringAttribute{MarkdownDescription: "Latest sync status.", Computed: true},
			"health_status": schema.StringAttribute{MarkdownDescription: "Latest health status.", Computed: true},
		},
	}
}

func (r *argoApplicationResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

// ValidateConfig checks combinations without contacting the cluster.
func (r *argoApplicationResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data argoApplicationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	source, sync, diags := argoParts(ctx, data)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateArgoApplication(source, sync, data.WaitForSync)...)
}

func argoParts(ctx context.Context, m argoApplicationModel) (argoSourceModel, argoSyncPolicyModel, diag.Diagnostics) {
	var source argoSourceModel
	var sync argoSyncPolicyModel
	var diags diag.Diagnostics
	if !m.Source.IsNull() && !m.Source.IsUnknown() {
		diags.Append(m.Source.As(ctx, &source, objectAsOptions())...)
	}
	if !m.SyncPolicy.IsNull() && !m.SyncPolicy.IsUnknown() {
		diags.Append(m.SyncPolicy.As(ctx, &sync, objectAsOptions())...)
	}
	return source, sync, diags
}

func validateArgoApplication(source argoSourceModel, sync argoSyncPolicyModel, wait types.Bool) diag.Diagnostics {
	var diags diag.Diagnostics
	sourcePath := path.Root("source")

	if !source.Path.IsUnknown() && !source.Chart.IsUnknown() && source.Path.IsNull() == source.Chart.IsNull() {
		diags.AddAttributeError(sourcePath, "Invalid source", "Set exactly one of path or chart.")
	}
	if !source.Chart.IsNull() && !source.DirectoryRecurse.IsNull() {
		diags.AddAttributeError(sourcePath, "Invalid source", "directory_recurse only applies to path.")
	}
	if source.TargetRevision.ValueString() == argoMovingRevision {
		diags.AddAttributeWarning(sourcePath, "Moving target revision",
			"HEAD deploys whatever Git holds at the time, so a rebuild may differ. Pin a commit or tag to keep it reproducible.")
	}
	if wait.ValueBool() && !sync.Automated.ValueBool() {
		diags.AddAttributeError(path.Root("wait_for_sync"), "Nothing to wait for", "wait_for_sync needs sync_policy.automated, because a manual application never syncs on its own.")
	}
	if !sync.Automated.ValueBool() && (sync.Prune.ValueBool() || sync.SelfHeal.ValueBool()) {
		diags.AddAttributeError(path.Root("sync_policy"), "Invalid sync policy", "prune and self_heal need automated.")
	}
	return diags
}

func argoRef(m argoApplicationModel) kube.Ref {
	return kube.Ref{APIVersion: argoAPIVersion, Kind: argoKind, Namespace: m.Namespace.ValueString(), Name: m.Name.ValueString()}
}

func buildArgoApplication(ctx context.Context, m argoApplicationModel) (*unstructured.Unstructured, diag.Diagnostics) {
	source, sync, diags := argoParts(ctx, m)
	var destination argoDestinationModel
	diags.Append(m.Destination.As(ctx, &destination, objectAsOptions())...)
	syncOptions, d := stringList(ctx, sync.SyncOptions)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}

	sourceSpec := map[string]any{
		"repoURL":        source.RepoURL.ValueString(),
		"targetRevision": source.TargetRevision.ValueString(),
	}
	if !source.Path.IsNull() {
		sourceSpec["path"] = source.Path.ValueString()
	}
	if !source.Chart.IsNull() {
		sourceSpec["chart"] = source.Chart.ValueString()
	}
	if source.DirectoryRecurse.ValueBool() {
		sourceSpec["directory"] = map[string]any{"recurse": true}
	}
	helmSpec := map[string]any{}
	if !source.HelmValues.IsNull() {
		helmSpec["values"] = source.HelmValues.ValueString()
	}
	if !source.HelmReleaseName.IsNull() {
		helmSpec["releaseName"] = source.HelmReleaseName.ValueString()
	}
	if len(helmSpec) > 0 {
		sourceSpec["helm"] = helmSpec
	}

	server := destination.Server.ValueString()
	if server == "" {
		server = argoDefaultServer
	}
	spec := map[string]any{
		"project":     m.Project.ValueString(),
		"source":      sourceSpec,
		"destination": map[string]any{"server": server, "namespace": destination.Namespace.ValueString()},
	}

	syncSpec := map[string]any{}
	if sync.Automated.ValueBool() {
		syncSpec["automated"] = map[string]any{"prune": sync.Prune.ValueBool(), "selfHeal": sync.SelfHeal.ValueBool()}
	}
	if len(syncOptions) > 0 {
		syncSpec["syncOptions"] = toAnySlice(syncOptions)
	}
	if len(syncSpec) > 0 {
		spec["syncPolicy"] = syncSpec
	}

	obj := newObject(argoAPIVersion, argoKind, m.Namespace.ValueString(), m.Name.ValueString())
	if m.CascadeDelete.ValueBool() {
		obj.SetFinalizers([]string{argoCascadeFinalizer})
	}
	obj.Object["spec"] = spec
	return obj, diags
}

func argoStatuses(obj *unstructured.Unstructured) (syncStatus, healthStatus string) {
	syncStatus, _, _ = unstructured.NestedString(obj.Object, "status", "sync", "status")
	healthStatus, _, _ = unstructured.NestedString(obj.Object, "status", "health", "status")
	if syncStatus == "" {
		syncStatus = argoStatusUnknown
	}
	if healthStatus == "" {
		healthStatus = argoStatusUnknown
	}
	return syncStatus, healthStatus
}

// argoSynced reports whether an application finished, and returns an error for
// a sync operation that failed, so the wait does not run to its timeout.
func argoSynced(obj *unstructured.Unstructured) (bool, error) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "operationState", "phase")
	if phase == "Failed" || phase == "Error" {
		message, _, _ := unstructured.NestedString(obj.Object, "status", "operationState", "message")
		return false, fmt.Errorf("sync %s: %s", phase, message)
	}
	syncStatus, healthStatus := argoStatuses(obj)
	return syncStatus == "Synced" && healthStatus == "Healthy", nil
}

func applyArgoApplication(ctx context.Context, client *kube.Client, plan *argoApplicationModel) diag.Diagnostics {
	var diags diag.Diagnostics

	obj, d := buildArgoApplication(ctx, *plan)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	timeout, err := parseTimeout(plan.Timeout, defaultWaitTimeout)
	if err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
		return diags
	}

	applied, err := client.Apply(ctx, obj)
	if err != nil {
		diags.AddError("Failed to apply the application", err.Error())
		return diags
	}
	plan.SyncStatus, plan.HealthStatus = statusValues(applied)

	if !plan.WaitForSync.ValueBool() {
		return diags
	}
	latest, err := client.WaitFor(ctx, argoRef(*plan), timeout, "application to be synced and healthy", argoSynced)
	if latest != nil {
		plan.SyncStatus, plan.HealthStatus = statusValues(latest)
	}
	if err != nil {
		diags.AddError("Application did not sync", err.Error())
	}
	return diags
}

func statusValues(obj *unstructured.Unstructured) (types.String, types.String) {
	syncStatus, healthStatus := argoStatuses(obj)
	return types.StringValue(syncStatus), types.StringValue(healthStatus)
}

func (r *argoApplicationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan argoApplicationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	exists, err := client.Exists(ctx, argoRef(plan))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the application", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Application already exists",
			fmt.Sprintf("%s already exists: import it so that it has one owner.", argoRef(plan)))
		return
	}

	plan.ID = types.StringValue(objectID(plan.Cluster, plan.Namespace.ValueString(), plan.Name.ValueString()))
	plan.SyncStatus = types.StringValue(argoStatusUnknown)
	plan.HealthStatus = types.StringValue(argoStatusUnknown)

	resp.Diagnostics.Append(applyArgoApplication(ctx, client, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *argoApplicationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data argoApplicationModel
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

	obj, err := client.Get(ctx, argoRef(data))
	if errors.Is(err, kube.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the application", err.Error())
		return
	}

	data.SyncStatus, data.HealthStatus = statusValues(obj)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func (r *argoApplicationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan argoApplicationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	diags := applyArgoApplication(ctx, client, &plan)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *argoApplicationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state argoApplicationModel
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
	if err := client.Delete(ctx, argoRef(state), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the application", err.Error())
	}
}

func (r *argoApplicationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importNamespaced(ctx, req, resp)
}
