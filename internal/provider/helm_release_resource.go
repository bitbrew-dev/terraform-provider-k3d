package provider

import (
	"context"
	"strconv"
	"strings"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	defaultReleaseTimeout = "10m"
	defaultMaxHistory     = 10
	statusDeployed        = "deployed"
	appliedRevisionKey    = "applied_revision"
)

type helmReleaseResource struct{}

type helmReleaseModel struct {
	ID                   types.String `tfsdk:"id"`
	Cluster              types.String `tfsdk:"cluster"`
	Kubeconfig           types.String `tfsdk:"kubeconfig"`
	Name                 types.String `tfsdk:"name"`
	Namespace            types.String `tfsdk:"namespace"`
	Repository           types.String `tfsdk:"repository"`
	Chart                types.String `tfsdk:"chart"`
	ChartVersion         types.String `tfsdk:"chart_version"`
	PlainHTTP            types.Bool   `tfsdk:"plain_http"`
	SrcPath              types.String `tfsdk:"src_path"`
	ExpectedChartDigest  types.String `tfsdk:"expected_chart_digest"`
	Values               types.List   `tfsdk:"values"`
	Set                  types.Map    `tfsdk:"set"`
	ValuesWO             types.List   `tfsdk:"values_wo"`
	ValuesWOVersion      types.Int64  `tfsdk:"values_wo_version"`
	CreateNamespace      types.Bool   `tfsdk:"create_namespace"`
	TakeOwnership        types.Bool   `tfsdk:"take_ownership"`
	Wait                 types.Bool   `tfsdk:"wait"`
	WaitForJobs          types.Bool   `tfsdk:"wait_for_jobs"`
	Atomic               types.Bool   `tfsdk:"atomic"`
	CleanupOnFail        types.Bool   `tfsdk:"cleanup_on_fail"`
	MaxHistory           types.Int64  `tfsdk:"max_history"`
	Timeout              types.String `tfsdk:"timeout"`
	ChartDigest          types.String `tfsdk:"chart_digest"`
	Revision             types.Int64  `tfsdk:"revision"`
	Status               types.String `tfsdk:"status"`
	DeployedChartVersion types.String `tfsdk:"deployed_chart_version"`
	AppVersion           types.String `tfsdk:"app_version"`
}

func (r *helmReleaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_helm_release"
}

func (r *helmReleaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	optionalBool := func(description string, value bool) schema.BoolAttribute {
		return schema.BoolAttribute{
			MarkdownDescription: description,
			Optional:            true,
			Computed:            true,
			Default:             booldefault.StaticBool(value),
		}
	}
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{MarkdownDescription: description, Computed: true}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "A Helm release. Set exactly one of `src_path` for a local chart folder, or `chart` for a chart from a repository or `oci://` registry, which also needs `chart_version`. " +
			"Every apply that changes something upgrades the release, and its values always match the configuration exactly. " +
			"Changes made outside Terraform, such as a manual `helm upgrade` or `helm rollback`, show up as a new revision, and the next apply restores the configuration. " +
			"Helm waits for resources before it runs `post-install` hooks, so `wait` hangs if those resources need the hook to become ready: use a regular Job or an init container. " +
			"Creating a release that already exists fails: import it instead, with `<cluster>/<namespace>/<name>`. Private `oci://` charts use credentials from `helm registry login`.",
		Attributes: map[string]schema.Attribute{
			"id":         computedString("`<cluster>/<namespace>/<name>`.", false),
			"cluster":    clusterAttribute(),
			"kubeconfig": kubeconfigAttribute(),
			"name":       requiredReplaceString("Release name."),
			"namespace":  namespaceAttribute(),
			"repository": schema.StringAttribute{
				MarkdownDescription: "URL of a classic chart repository. Not used with `oci://` charts.",
				Optional:            true,
			},
			"chart": schema.StringAttribute{
				MarkdownDescription: "Chart name in `repository`, or a full `oci://` reference.",
				Optional:            true,
			},
			"chart_version": schema.StringAttribute{
				MarkdownDescription: "Exact chart version. Required with `chart`, so a release is reproducible.",
				Optional:            true,
			},
			"plain_http": optionalBool("Use HTTP instead of HTTPS for an `oci://` registry, such as a local `k3d_registry`. Defaults to `false`.", false),
			"src_path": schema.StringAttribute{
				MarkdownDescription: "Path to a local chart folder on the machine running Terraform. Use `abspath(\"${path.module}/...\")` so it does not depend on the working directory. " +
					"A chart with dependencies needs `Chart.lock` and a built `charts/` folder.",
				Optional: true,
			},
			"expected_chart_digest": schema.StringAttribute{
				MarkdownDescription: "Fail the plan unless the local chart has this `chart_digest`, which pins it to known content.",
				Optional:            true,
			},
			"values": schema.ListAttribute{
				MarkdownDescription: "YAML documents, merged in order with later ones winning.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"set": schema.MapAttribute{
				MarkdownDescription: "Individual values, applied after every document. Keys and types follow Helm's `--set` rules, so `\"3\"` becomes a number.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"values_wo": schema.ListAttribute{
				MarkdownDescription: "YAML documents that never reach Terraform plan or state, merged after `values`. Bump `values_wo_version` to apply changes. Helm still stores them in its release Secret in the cluster. Needs Terraform 1.11 or later.",
				ElementType:         types.StringType,
				Optional:            true,
				WriteOnly:           true,
				Validators:          []validator.List{listvalidator.AlsoRequires(path.MatchRoot("values_wo_version"))},
			},
			"values_wo_version": schema.Int64Attribute{
				MarkdownDescription: "Change this to apply the current `values_wo`.",
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AlsoRequires(path.MatchRoot("values_wo"))},
			},
			"create_namespace": optionalBool("Create the namespace if it is missing. It is not removed with the release. Defaults to `false`.", false),
			"take_ownership":   optionalBool("Let the release adopt existing objects its chart defines, such as a namespace created earlier. Defaults to `false`.", false),
			"wait":             optionalBool("Wait for the release's resources to be ready. Defaults to `true`.", true),
			"wait_for_jobs":    optionalBool("With `wait`, also wait for Jobs to complete. Defaults to `false`.", false),
			"atomic":           optionalBool("Undo a failed install or upgrade. Defaults to `false`.", false),
			"cleanup_on_fail":  optionalBool("Delete objects a failed upgrade created. Defaults to `false`.", false),
			"max_history": schema.Int64Attribute{
				MarkdownDescription: "Revisions to keep. Defaults to 10.",
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(defaultMaxHistory),
				Validators:          []validator.Int64{int64validator.AtLeast(0)},
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "How long an install, upgrade, or uninstall can take. Defaults to `10m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultReleaseTimeout),
				Validators:          []validator.String{duration()},
			},
			"chart_digest": computed("Digest of a local chart, null for remote charts. Computed during plan, so an edited chart shows as a change."),
			"revision": schema.Int64Attribute{
				MarkdownDescription: "Current revision of the release.",
				Computed:            true,
			},
			"status":                 computed("Current status of the release."),
			"deployed_chart_version": computed("Version of the chart that is deployed."),
			"app_version":            computed("App version of the chart that is deployed."),
		},
	}
}

func (r *helmReleaseResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return append(connectionValidators(),
		resourcevalidator.ExactlyOneOf(path.MatchRoot("src_path"), path.MatchRoot("chart")),
	)
}

// ValidateConfig checks the combinations a source needs. It reads nothing, so a
// source built from another resource is checked during plan instead.
func (r *helmReleaseResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data helmReleaseModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateChartSource(data)...)
}

func validateChartSource(m helmReleaseModel) diag.Diagnostics {
	var diags diag.Diagnostics
	set := func(s types.String) bool { return !s.IsNull() }

	if set(m.SrcPath) {
		for name, value := range map[string]types.String{"repository": m.Repository, "chart_version": m.ChartVersion} {
			if set(value) {
				diags.AddAttributeError(path.Root(name), "Invalid chart source", name+" cannot be used with src_path.")
			}
		}
		return diags
	}
	if set(m.ExpectedChartDigest) {
		diags.AddAttributeError(path.Root("expected_chart_digest"), "Invalid chart source", "expected_chart_digest only applies to src_path.")
	}
	if m.Chart.IsNull() || m.Chart.IsUnknown() {
		return diags
	}

	if !set(m.ChartVersion) {
		diags.AddAttributeError(path.Root("chart_version"), "Missing chart version", "chart_version is required with chart, so the release is reproducible.")
	}
	isOCI := strings.HasPrefix(m.Chart.ValueString(), "oci://")
	if isOCI && set(m.Repository) {
		diags.AddAttributeError(path.Root("repository"), "Invalid chart source", "repository cannot be used with an oci:// chart.")
	}
	if !isOCI && !set(m.Repository) {
		diags.AddAttributeError(path.Root("repository"), "Missing repository", "repository is required unless chart is an oci:// reference.")
	}
	return diags
}

// ModifyPlan needs only the local filesystem. It computes the digest of a local
// chart, so an edited chart plans as an upgrade, and turns a release changed
// outside Terraform into an upgrade that restores the configuration.
func (r *helmReleaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan helmReleaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	digest, diags := planChartDigest(plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("chart_digest"), digest)...)

	if req.State.Raw.IsNull() {
		return
	}
	var state helmReleaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	applied, d := req.Private.GetKey(ctx, appliedRevisionKey)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	if digest.Equal(state.ChartDigest) && !releaseDrifted(state, string(applied)) {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("revision"), types.Int64Unknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("deployed_chart_version"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("app_version"), types.StringUnknown())...)
}

// releaseDrifted reports a release that is not what Terraform last applied. An
// imported release has no applied revision yet, so only its status counts.
func releaseDrifted(state helmReleaseModel, appliedRevision string) bool {
	if state.Status.ValueString() != statusDeployed {
		return true
	}
	return appliedRevision != "" && appliedRevision != strconv.FormatInt(state.Revision.ValueInt64(), 10)
}

func planChartDigest(m helmReleaseModel) (types.String, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m.SrcPath.IsUnknown() {
		return types.StringUnknown(), diags
	}
	if m.SrcPath.IsNull() {
		return types.StringNull(), diags
	}

	chartPath := m.SrcPath.ValueString()
	if err := helm.CheckLocal(chartPath); err != nil {
		diags.AddAttributeError(path.Root("src_path"), "Invalid chart", err.Error())
		return types.StringUnknown(), diags
	}
	digest, err := helm.LocalDigest(chartPath)
	if err != nil {
		diags.AddAttributeError(path.Root("src_path"), "Failed to read the chart", err.Error())
		return types.StringUnknown(), diags
	}
	if !m.ExpectedChartDigest.IsNull() && !m.ExpectedChartDigest.IsUnknown() && m.ExpectedChartDigest.ValueString() != digest {
		diags.AddAttributeError(path.Root("expected_chart_digest"), "Chart digest mismatch",
			"The chart at "+chartPath+" has digest "+digest+", not "+m.ExpectedChartDigest.ValueString()+". Check out the expected version of the chart, or update expected_chart_digest.")
		return types.StringUnknown(), diags
	}
	return types.StringValue(digest), diags
}
