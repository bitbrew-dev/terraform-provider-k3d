package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	chart "helm.sh/helm/v4/pkg/chart"
)

var (
	_ resource.Resource                     = (*helmReleaseResource)(nil)
	_ resource.ResourceWithImportState      = (*helmReleaseResource)(nil)
	_ resource.ResourceWithConfigValidators = (*helmReleaseResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*helmReleaseResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*helmReleaseResource)(nil)
)

// NewHelmReleaseResource returns the k3d_helm_release resource.
func NewHelmReleaseResource() resource.Resource {
	return &helmReleaseResource{}
}

func releaseClient(ctx context.Context, m helmReleaseModel) (*helm.Client, error) {
	raw, err := resolveKubeconfig(ctx, m.Cluster, m.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return helm.New(raw, m.Namespace.ValueString())
}

// prepareRelease loads the chart and merges the values. Write-only values exist
// only in the configuration, never in the plan.
func prepareRelease(ctx context.Context, plan, config helmReleaseModel) (chart.Charter, map[string]any, helm.Options, diag.Diagnostics) {
	var options helm.Options

	timeout, err := parseTimeout(plan.Timeout, defaultReleaseTimeout)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
		return nil, nil, options, diags
	}

	documents, diags := stringList(ctx, plan.Values)
	writeOnly, d := stringList(ctx, config.ValuesWO)
	diags.Append(d...)
	set, d := stringMap(ctx, plan.Set)
	diags.Append(d...)
	if diags.HasError() {
		return nil, nil, options, diags
	}

	values, err := helm.MergeValues(append(documents, writeOnly...), set)
	if err != nil {
		diags.AddError("Invalid values", err.Error())
		return nil, nil, options, diags
	}

	ch, err := chartSource(plan)
	if err != nil {
		diags.AddError("Failed to load the chart", err.Error())
		return nil, nil, options, diags
	}

	return ch, values, helm.Options{
		Name:            plan.Name.ValueString(),
		Namespace:       plan.Namespace.ValueString(),
		Wait:            plan.Wait.ValueBool(),
		WaitForJobs:     plan.WaitForJobs.ValueBool(),
		Atomic:          plan.Atomic.ValueBool(),
		CleanupOnFail:   plan.CleanupOnFail.ValueBool(),
		CreateNamespace: plan.CreateNamespace.ValueBool(),
		TakeOwnership:   plan.TakeOwnership.ValueBool(),
		MaxHistory:      int(plan.MaxHistory.ValueInt64()),
		Timeout:         timeout,
	}, diags
}

// chartSource loads the chart, refusing a local chart that changed after the
// plan, because the plan shown to the user no longer describes it.
func chartSource(m helmReleaseModel) (chart.Charter, error) {
	if !m.SrcPath.IsNull() {
		digest, err := helm.LocalDigest(m.SrcPath.ValueString())
		if err != nil {
			return nil, err
		}
		if digest != m.ChartDigest.ValueString() {
			return nil, fmt.Errorf("chart at %s changed after the plan: run plan again", m.SrcPath.ValueString())
		}
	}
	return helm.Source{
		Path:       m.SrcPath.ValueString(),
		Repository: m.Repository.ValueString(),
		Chart:      m.Chart.ValueString(),
		Version:    m.ChartVersion.ValueString(),
		PlainHTTP:  m.PlainHTTP.ValueBool(),
	}.Load()
}

func applyReleaseState(m *helmReleaseModel, rel *helm.Release) {
	m.ID = types.StringValue(objectID(m.Cluster, rel.Namespace, rel.Name))
	m.Revision = types.Int64Value(int64(rel.Revision))
	m.Status = types.StringValue(rel.Status)
	m.DeployedChartVersion = types.StringValue(rel.ChartVersion)
	m.AppVersion = types.StringValue(rel.AppVersion)
	m.ValuesWO = types.ListNull(types.StringType)
}

func appliedRevision(rel *helm.Release) []byte {
	return []byte(strconv.Itoa(rel.Revision))
}

func (r *helmReleaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config helmReleaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := releaseClient(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	name := plan.Name.ValueString()
	_, err = client.Get(name)
	if err == nil {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Release already exists",
			"Release "+name+" already exists: import it so that it has one owner.")
		return
	}
	if !errors.Is(err, helm.ErrNotFound) {
		resp.Diagnostics.AddError("Failed to check the release", err.Error())
		return
	}

	ch, values, options, diags := prepareRelease(ctx, plan, config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	rel, err := client.Install(ctx, ch, values, options)
	if rel != nil {
		applyReleaseState(&plan, rel)
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedRevisionKey, appliedRevision(rel))...)
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to install the release", err.Error())
	}
}

func (r *helmReleaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data helmReleaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := releaseClient(ctx, data)
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	rel, err := client.Get(data.Name.ValueString())
	if errors.Is(err, helm.ErrNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the release", err.Error())
		return
	}

	applyReleaseState(&data, rel)
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// Update upgrades the release. On failure it leaves state as it was, so the
// release's failed status plans another upgrade.
func (r *helmReleaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config helmReleaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := releaseClient(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	ch, values, options, diags := prepareRelease(ctx, plan, config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	rel, err := client.Upgrade(ctx, ch, values, options)
	if err != nil {
		resp.Diagnostics.AddError("Failed to upgrade the release", err.Error())
		return
	}

	applyReleaseState(&plan, rel)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedRevisionKey, appliedRevision(rel))...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *helmReleaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state helmReleaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := releaseClient(ctx, state)
	if isGone(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	timeout, err := parseTimeout(state.Timeout, defaultReleaseTimeout)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "Invalid timeout", err.Error())
		return
	}
	if err := client.Uninstall(state.Name.ValueString(), state.Wait.ValueBool(), timeout); err != nil {
		resp.Diagnostics.AddError("Failed to uninstall the release", err.Error())
	}
}

// ImportState imports with <cluster>/<namespace>/<name>. The next apply upgrades
// the release to match the configuration, without replacing it.
func (r *helmReleaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importNamespaced(ctx, req, resp)
}
