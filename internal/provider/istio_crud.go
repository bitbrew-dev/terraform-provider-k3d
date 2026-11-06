package provider

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*istioResource)(nil)
	_ resource.ResourceWithImportState      = (*istioResource)(nil)
	_ resource.ResourceWithConfigValidators = (*istioResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*istioResource)(nil)
)

// NewIstioResource returns the k3d_istio resource.
func NewIstioResource() resource.Resource {
	return &istioResource{}
}

func istioClient(ctx context.Context, m istioModel) (*helm.Client, error) {
	raw, err := resolveKubeconfig(ctx, m.Cluster, m.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return helm.New(raw, m.Namespace.ValueString())
}

func istioOptions(m istioModel, release string) (helm.Options, error) {
	timeout, err := parseTimeout(m.Timeout, defaultReleaseTimeout)
	if err != nil {
		return helm.Options{}, err
	}
	return helm.Options{
		Name:          release,
		Namespace:     m.Namespace.ValueString(),
		Wait:          true,
		TakeOwnership: m.TakeOwnership.ValueBool(),
		MaxHistory:    defaultMaxHistory,
		Timeout:       timeout,
	}, nil
}

// applyIstio installs or upgrades every component in order. The order matters:
// istiod needs the CRDs from base, and a gateway needs istiod's injection webhook.
func applyIstio(ctx context.Context, client *helm.Client, m istioModel, components []istioComponent) error {
	for _, component := range components {
		ch, err := helm.Source{
			Repository: m.Repository.ValueString(),
			Chart:      component.Chart,
			Version:    m.Version.ValueString(),
		}.Load()
		if err != nil {
			return err
		}
		options, err := istioOptions(m, component.Release)
		if err != nil {
			return err
		}

		_, err = client.Get(component.Release)
		switch {
		case errors.Is(err, helm.ErrNotFound):
			_, err = client.Install(ctx, ch, component.Values, options)
		case err == nil:
			_, err = client.Upgrade(ctx, ch, component.Values, options)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// refreshIstio reads every component into the model and reports whether any
// component exists at all. A missing one reads as revision 0, which plans a
// restore of it.
func refreshIstio(ctx context.Context, client *helm.Client, m *istioModel) (bool, error) {
	gateway, diags := gatewayOf(ctx, *m)
	if diags.HasError() {
		return false, diagsError(diags)
	}

	found := false
	status := statusDeployed
	read := func(release string) (types.Int64, error) {
		rel, err := client.Get(release)
		if errors.Is(err, helm.ErrNotFound) {
			if status == statusDeployed {
				status = istioMissing
			}
			return types.Int64Value(0), nil
		}
		if err != nil {
			return types.Int64Null(), err
		}
		found = true
		if status == statusDeployed && rel.Status != statusDeployed {
			status = rel.Status
		}
		return types.Int64Value(int64(rel.Revision)), nil
	}

	var err error
	if m.BaseRevision, err = read(istioBaseRelease); err != nil {
		return false, err
	}
	if m.IstiodRevision, err = read(istiodRelease); err != nil {
		return false, err
	}
	m.GatewayRevision = types.Int64Null()
	if gateway != nil {
		if m.GatewayRevision, err = read(gatewayRelease(gateway)); err != nil {
			return false, err
		}
	}
	m.Status = types.StringValue(status)
	m.ID = istioID(*m)
	return found, nil
}

func appliedIstioRevisions(m istioModel) []byte {
	encoded, err := json.Marshal(istioRevisions{
		Base:    m.BaseRevision.ValueInt64(),
		Istiod:  m.IstiodRevision.ValueInt64(),
		Gateway: m.GatewayRevision.ValueInt64(),
	})
	if err != nil {
		return nil
	}
	return encoded
}

func (r *istioResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan istioModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := istioClient(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	components, diags := istioComponents(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	for _, component := range components {
		_, err := client.Get(component.Release)
		if err == nil {
			resp.Diagnostics.AddError("Istio already installed",
				"Release "+component.Release+" already exists: import it so that it has one owner.")
			return
		}
		if !errors.Is(err, helm.ErrNotFound) {
			resp.Diagnostics.AddError("Failed to check for Istio", err.Error())
			return
		}
	}

	applyErr := applyIstio(ctx, client, plan, components)

	found, err := refreshIstio(ctx, client, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Istio", err.Error())
		return
	}
	if found {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedIstioRevisionKey, appliedIstioRevisions(plan))...)
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
	}
	if applyErr != nil {
		resp.Diagnostics.AddError("Failed to install Istio", applyErr.Error())
	}
}

func (r *istioResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data istioModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := istioClient(ctx, data)
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	found, err := refreshIstio(ctx, client, &data)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Istio", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

// Update upgrades every component. On failure it leaves state as it was, so the
// next refresh sees what changed and plans another upgrade.
func (r *istioResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state istioModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := istioClient(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	components, diags := istioComponents(ctx, plan)
	resp.Diagnostics.Append(diags...)
	oldGateway, d := gatewayOf(ctx, state)
	resp.Diagnostics.Append(d...)
	newGateway, d := gatewayOf(ctx, plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	if old := gatewayRelease(oldGateway); old != "" && old != gatewayRelease(newGateway) {
		options, err := istioOptions(plan, old)
		if err != nil {
			resp.Diagnostics.AddError("Invalid timeout", err.Error())
			return
		}
		if err := client.Uninstall(old, true, options.Timeout); err != nil {
			resp.Diagnostics.AddError("Failed to remove the old gateway", err.Error())
			return
		}
	}

	if err := applyIstio(ctx, client, plan, components); err != nil {
		resp.Diagnostics.AddError("Failed to upgrade Istio", err.Error())
		return
	}

	if _, err := refreshIstio(ctx, client, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to read Istio", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedIstioRevisionKey, appliedIstioRevisions(plan))...)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete removes components in reverse order, so that nothing is left depending
// on the webhook while it goes away.
func (r *istioResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state istioModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	gateway, d := gatewayOf(ctx, state)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := istioClient(ctx, state)
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
	releases := []string{istiodRelease, istioBaseRelease}
	if release := gatewayRelease(gateway); release != "" {
		releases = append([]string{release}, releases...)
	}
	for _, release := range releases {
		if err := client.Uninstall(release, true, timeout); err != nil {
			resp.Diagnostics.AddError("Failed to uninstall Istio", err.Error())
			return
		}
	}
}

// ImportState imports with <cluster>/<namespace>. The next apply upgrades the
// components to match the configuration, and installs any gateway it declares.
func (r *istioResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, err := splitImportID(req.ID, "<cluster>/<namespace>", 2)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), parts[1])...)
}
