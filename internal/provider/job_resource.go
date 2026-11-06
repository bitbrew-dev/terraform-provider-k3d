package provider

import (
	"context"
	"errors"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const defaultBackoffLimit = 6

var (
	_ resource.Resource                     = (*jobResource)(nil)
	_ resource.ResourceWithImportState      = (*jobResource)(nil)
	_ resource.ResourceWithConfigValidators = (*jobResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*jobResource)(nil)
)

// NewJobResource returns the k3d_job resource.
func NewJobResource() resource.Resource {
	return &jobResource{}
}

type jobResource struct{}

type jobModel struct {
	ID                 types.String `tfsdk:"id"`
	Cluster            types.String `tfsdk:"cluster"`
	Kubeconfig         types.String `tfsdk:"kubeconfig"`
	Name               types.String `tfsdk:"name"`
	Namespace          types.String `tfsdk:"namespace"`
	Labels             types.Map    `tfsdk:"labels"`
	Annotations        types.Map    `tfsdk:"annotations"`
	PodAnnotations     types.Map    `tfsdk:"pod_annotations"`
	Image              types.String `tfsdk:"image"`
	Command            types.List   `tfsdk:"command"`
	Args               types.List   `tfsdk:"args"`
	Env                types.Map    `tfsdk:"env"`
	ServiceAccountName types.String `tfsdk:"service_account_name"`
	ImagePullSecrets   types.List   `tfsdk:"image_pull_secrets"`
	BackoffLimit       types.Int64  `tfsdk:"backoff_limit"`
	TTLSeconds         types.Int64  `tfsdk:"ttl_seconds_after_finished"`
	Wait               types.Bool   `tfsdk:"wait"`
	Timeout            types.String `tfsdk:"timeout"`
}

func jobRef(namespace, name string) kube.Ref {
	return kube.Ref{APIVersion: "batch/v1", Kind: "Job", Namespace: namespace, Name: name}
}

func (r *jobResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_job"
}

func (r *jobResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Kubernetes Job with one container. Jobs are immutable, so any change except `wait` and `timeout` replaces and re-runs it. " +
			"A failed job is tainted and replaced by the next apply. For volumes or several containers, use `k3d_manifest`. Do not use this for a Helm chart's hook jobs. " +
			"In a namespace with Istio injection the sidecar keeps the job running forever, so set `pod_annotations = { \"sidecar.istio.io/inject\" = \"false\" }`. " +
			"Import with `<cluster>/<namespace>/<name>`.",
		Attributes: map[string]schema.Attribute{
			"id":              computedString("`<cluster>/<namespace>/<name>`.", false),
			"cluster":         clusterAttribute(),
			"kubeconfig":      kubeconfigAttribute(),
			"name":            requiredReplaceString("Job name."),
			"namespace":       namespaceAttribute(),
			"labels":          schema.MapAttribute{MarkdownDescription: "Labels on the job.", ElementType: types.StringType, Optional: true},
			"annotations":     schema.MapAttribute{MarkdownDescription: "Annotations on the job.", ElementType: types.StringType, Optional: true},
			"pod_annotations": schema.MapAttribute{MarkdownDescription: "Annotations on the job's pods.", ElementType: types.StringType, Optional: true},
			"image": schema.StringAttribute{
				MarkdownDescription: "Container image with a pinned tag or digest.",
				Required:            true,
				Validators:          []validator.String{pinnedImage()},
			},
			"command":              schema.ListAttribute{MarkdownDescription: "Container command.", ElementType: types.StringType, Optional: true},
			"args":                 schema.ListAttribute{MarkdownDescription: "Container arguments.", ElementType: types.StringType, Optional: true},
			"env":                  schema.MapAttribute{MarkdownDescription: "Environment variables.", ElementType: types.StringType, Optional: true},
			"service_account_name": schema.StringAttribute{MarkdownDescription: "Service account for the pods.", Optional: true},
			"image_pull_secrets":   schema.ListAttribute{MarkdownDescription: "Names of pull secrets.", ElementType: types.StringType, Optional: true},
			"backoff_limit": schema.Int64Attribute{
				MarkdownDescription: "Retries before the job fails. Defaults to 6.",
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(defaultBackoffLimit),
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"ttl_seconds_after_finished": schema.Int64Attribute{
				MarkdownDescription: "Delete the job this long after it finishes. Terraform keeps it in state afterwards, so it is not re-run. With `wait`, use at least 10 seconds: a job removed before its result is seen is reported as an error, because it may have failed.",
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"wait": schema.BoolAttribute{
				MarkdownDescription: "Wait for the job to finish. Defaults to `true`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "How long to wait. Defaults to `5m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultWaitTimeout),
				Validators:          []validator.String{duration()},
			},
		},
	}
}

func (r *jobResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return connectionValidators()
}

// ModifyPlan replaces the job on any change to its immutable fields. After an
// import they are adopted, because they are not read back from the cluster.
func (r *jobResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var state, plan jobModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	imported, diags := isImported(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.RequiresReplace = append(resp.RequiresReplace, replacementPaths([]createOnly{
		{"labels", state.Labels, plan.Labels},
		{"annotations", state.Annotations, plan.Annotations},
		{"pod_annotations", state.PodAnnotations, plan.PodAnnotations},
		{"image", state.Image, plan.Image},
		{"command", state.Command, plan.Command},
		{"args", state.Args, plan.Args},
		{"env", state.Env, plan.Env},
		{"service_account_name", state.ServiceAccountName, plan.ServiceAccountName},
		{"image_pull_secrets", state.ImagePullSecrets, plan.ImagePullSecrets},
		{"backoff_limit", state.BackoffLimit, plan.BackoffLimit},
		{"ttl_seconds_after_finished", state.TTLSeconds, plan.TTLSeconds},
	}, imported)...)
}

func buildJob(ctx context.Context, m jobModel) (*unstructured.Unstructured, diag.Diagnostics) {
	obj := newObject("batch/v1", "Job", m.Namespace.ValueString(), m.Name.ValueString())
	diags := setMetadata(ctx, obj, m.Labels, m.Annotations)

	container := map[string]any{"name": "main", "image": m.Image.ValueString()}

	command, d := stringList(ctx, m.Command)
	diags.Append(d...)
	if len(command) > 0 {
		container["command"] = toAnySlice(command)
	}
	args, d := stringList(ctx, m.Args)
	diags.Append(d...)
	if len(args) > 0 {
		container["args"] = toAnySlice(args)
	}
	env, d := stringMap(ctx, m.Env)
	diags.Append(d...)
	if len(env) > 0 {
		vars := make([]any, 0, len(env))
		for _, name := range sortedKeys(env) {
			vars = append(vars, map[string]any{"name": name, "value": env[name]})
		}
		container["env"] = vars
	}

	podSpec := map[string]any{
		"restartPolicy": "Never",
		"containers":    []any{container},
	}
	if name := m.ServiceAccountName.ValueString(); name != "" {
		podSpec["serviceAccountName"] = name
	}
	pullSecrets, d := stringList(ctx, m.ImagePullSecrets)
	diags.Append(d...)
	if len(pullSecrets) > 0 {
		refs := make([]any, 0, len(pullSecrets))
		for _, name := range pullSecrets {
			refs = append(refs, map[string]any{"name": name})
		}
		podSpec["imagePullSecrets"] = refs
	}

	template := map[string]any{"spec": podSpec}
	podAnnotations, d := stringMap(ctx, m.PodAnnotations)
	diags.Append(d...)
	if len(podAnnotations) > 0 {
		values := make(map[string]any, len(podAnnotations))
		for k, v := range podAnnotations {
			values[k] = v
		}
		template["metadata"] = map[string]any{"annotations": values}
	}

	spec := map[string]any{
		"backoffLimit": m.BackoffLimit.ValueInt64(),
		"template":     template,
	}
	if !m.TTLSeconds.IsNull() {
		spec["ttlSecondsAfterFinished"] = m.TTLSeconds.ValueInt64()
	}
	obj.Object["spec"] = spec

	return obj, diags
}

func (r *jobResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan jobModel
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

	namespace, name := plan.Namespace.ValueString(), plan.Name.ValueString()
	exists, err := client.Exists(ctx, jobRef(namespace, name))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the job", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Job already exists",
			"Job "+namespace+"/"+name+" already exists: import it so that it has one owner.")
		return
	}

	obj, diags := buildJob(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := client.Apply(ctx, obj); err != nil {
		resp.Diagnostics.AddError("Failed to create the job", err.Error())
		return
	}

	plan.ID = types.StringValue(objectID(plan.Cluster, namespace, name))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	if resp.Diagnostics.HasError() || !plan.Wait.ValueBool() {
		return
	}

	err = client.WaitJob(ctx, namespace, name, timeout)
	if errors.Is(err, kube.ErrNotFound) && !plan.TTLSeconds.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("ttl_seconds_after_finished"), "Job result unknown",
			"The job was removed by ttl_seconds_after_finished before its result was seen, so it may have failed. Use at least 10 seconds, or set wait to false.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Job did not complete", err.Error())
	}
}

func (r *jobResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data jobModel
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

	_, err = client.Get(ctx, jobRef(data.Namespace.ValueString(), data.Name.ValueString()))
	if isGone(err) {
		if data.TTLSeconds.IsNull() {
			resp.State.RemoveResource(ctx)
		}
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the job", err.Error())
	}
}

// Update changes only wait and timeout, or adopts config after an import.
func (r *jobResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan jobModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(clearImported(ctx, resp.Private)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *jobResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state jobModel
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
	if err := client.Delete(ctx, jobRef(state.Namespace.ValueString(), state.Name.ValueString()), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the job", err.Error())
	}
}

func (r *jobResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importNamespaced(ctx, req, resp)
	resp.Diagnostics.Append(markImported(ctx, resp.Private)...)
}
