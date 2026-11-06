package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	testArgoChartVersion = "10.14.0"
	testGuestbookRepo    = "https://github.com/argoproj/argocd-example-apps.git"
	testGuestbookCommit  = "8088f4c0d970abb09e250248cc97e35623447cb5"
)

func TestBuildArgoApplication(t *testing.T) {
	ctx := context.Background()
	source, diags := types.ObjectValueFrom(ctx, map[string]attr.Type{
		"repo_url":          types.StringType,
		"target_revision":   types.StringType,
		"path":              types.StringType,
		"chart":             types.StringType,
		"directory_recurse": types.BoolType,
		"helm_values":       types.StringType,
		"helm_release_name": types.StringType,
	}, argoSourceModel{
		RepoURL:          types.StringValue("https://r"),
		TargetRevision:   types.StringValue("abc"),
		Path:             types.StringValue("dags"),
		Chart:            types.StringNull(),
		DirectoryRecurse: types.BoolValue(true),
		HelmValues:       types.StringNull(),
		HelmReleaseName:  types.StringNull(),
	})
	if diags.HasError() {
		t.Fatal(diags)
	}
	destination, diags := types.ObjectValueFrom(ctx, map[string]attr.Type{
		"server":    types.StringType,
		"namespace": types.StringType,
	}, argoDestinationModel{Server: types.StringNull(), Namespace: types.StringValue("airflow")})
	if diags.HasError() {
		t.Fatal(diags)
	}
	options, _ := types.ListValueFrom(ctx, types.StringType, []string{"CreateNamespace=true"})
	syncPolicy, diags := types.ObjectValueFrom(ctx, map[string]attr.Type{
		"automated":    types.BoolType,
		"prune":        types.BoolType,
		"self_heal":    types.BoolType,
		"sync_options": types.ListType{ElemType: types.StringType},
	}, argoSyncPolicyModel{
		Automated:   types.BoolValue(true),
		Prune:       types.BoolValue(true),
		SelfHeal:    types.BoolValue(true),
		SyncOptions: options,
	})
	if diags.HasError() {
		t.Fatal(diags)
	}

	obj, diags := buildArgoApplication(ctx, argoApplicationModel{
		Name:          types.StringValue("airflow-dags"),
		Namespace:     types.StringValue("argocd"),
		Project:       types.StringValue("default"),
		Source:        source,
		Destination:   destination,
		SyncPolicy:    syncPolicy,
		CascadeDelete: types.BoolValue(true),
	})
	if diags.HasError() {
		t.Fatal(diags)
	}

	checks := map[string][]string{
		"https://r":       {"spec", "source", "repoURL"},
		"abc":             {"spec", "source", "targetRevision"},
		"dags":            {"spec", "source", "path"},
		"airflow":         {"spec", "destination", "namespace"},
		argoDefaultServer: {"spec", "destination", "server"},
	}
	for want, fields := range checks {
		if got, _, _ := unstructured.NestedString(obj.Object, fields...); got != want {
			t.Errorf("%v = %q, want %q", fields, got, want)
		}
	}
	if recurse, _, _ := unstructured.NestedBool(obj.Object, "spec", "source", "directory", "recurse"); !recurse {
		t.Error("directory recurse not set")
	}
	if selfHeal, _, _ := unstructured.NestedBool(obj.Object, "spec", "syncPolicy", "automated", "selfHeal"); !selfHeal {
		t.Error("selfHeal not set")
	}
	if got := obj.GetFinalizers(); len(got) != 1 || got[0] != argoCascadeFinalizer {
		t.Errorf("finalizers = %v", got)
	}
}

func TestValidateArgoApplication(t *testing.T) {
	str := types.StringValue
	null := types.StringNull()
	nullBool := types.BoolNull()
	source := func(path, chart types.String, revision string) argoSourceModel {
		return argoSourceModel{Path: path, Chart: chart, TargetRevision: str(revision), DirectoryRecurse: nullBool}
	}
	automated := argoSyncPolicyModel{Automated: types.BoolValue(true), Prune: nullBool, SelfHeal: nullBool}
	manual := argoSyncPolicyModel{Automated: nullBool, Prune: nullBool, SelfHeal: nullBool}

	tests := []struct {
		name     string
		source   argoSourceModel
		sync     argoSyncPolicyModel
		wait     bool
		wantErr  bool
		wantWarn bool
	}{
		{name: "git path", source: source(str("p"), null, "abc"), sync: manual},
		{name: "helm chart", source: source(null, str("c"), "1.0.0"), sync: manual},
		{name: "neither", source: source(null, null, "abc"), sync: manual, wantErr: true},
		{name: "both", source: source(str("p"), str("c"), "abc"), sync: manual, wantErr: true},
		{name: "moving revision warns", source: source(str("p"), null, "HEAD"), sync: manual, wantWarn: true},
		{name: "wait with automated", source: source(str("p"), null, "abc"), sync: automated, wait: true},
		{name: "wait without automated", source: source(str("p"), null, "abc"), sync: manual, wait: true, wantErr: true},
		{
			name:    "prune without automated",
			source:  source(str("p"), null, "abc"),
			sync:    argoSyncPolicyModel{Automated: nullBool, Prune: types.BoolValue(true), SelfHeal: nullBool},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := validateArgoApplication(tt.source, tt.sync, types.BoolValue(tt.wait))
			if diags.HasError() != tt.wantErr {
				t.Errorf("HasError = %v: %v", diags.HasError(), diags)
			}
			if (diags.WarningsCount() > 0) != tt.wantWarn {
				t.Errorf("warnings = %d, want %v", diags.WarningsCount(), tt.wantWarn)
			}
		})
	}
}

func TestArgoSynced(t *testing.T) {
	app := func(sync, health, phase string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{
			"sync":           map[string]any{"status": sync},
			"health":         map[string]any{"status": health},
			"operationState": map[string]any{"phase": phase, "message": "boom"},
		}}}
	}

	tests := []struct {
		name    string
		obj     *unstructured.Unstructured
		want    bool
		wantErr bool
	}{
		{name: "no status yet", obj: &unstructured.Unstructured{Object: map[string]any{}}},
		{name: "syncing", obj: app("OutOfSync", "Missing", "Running")},
		{name: "synced but progressing", obj: app("Synced", "Progressing", "Succeeded")},
		{name: "done", obj: app("Synced", "Healthy", "Succeeded"), want: true},
		{name: "failed", obj: app("OutOfSync", "Missing", "Failed"), wantErr: true},
		{name: "error", obj: app("OutOfSync", "Missing", "Error"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := argoSynced(tt.obj)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("got (%v, %v), want (%v, wantErr %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestAccArgoApplication_lifecycle installs Argo CD from its chart and syncs a
// real application at a pinned commit. It needs internet access, for GitHub and
// for the chart and images.
func TestAccArgoApplication_lifecycle(t *testing.T) {
	cluster := testAccSharedCluster(t)
	address := "k3d_argocd_application.guestbook"

	platform := fmt.Sprintf(`
resource "k3d_namespace" "argocd" {
  cluster = %q
  name    = "argocd"
}

resource "k3d_helm_release" "argocd" {
  cluster       = %q
  namespace     = k3d_namespace.argocd.name
  name          = "argocd"
  repository    = "https://argoproj.github.io/argo-helm"
  chart         = "argo-cd"
  chart_version = %q
  timeout       = "6m"
  values = [yamlencode({
    dex            = { enabled = false }
    notifications  = { enabled = false }
    applicationSet = { enabled = false }
  })]
}
`, cluster, cluster, testArgoChartVersion)

	application := func(options string) string {
		return fmt.Sprintf(`
resource "k3d_argocd_application" "guestbook" {
  cluster       = %q
  namespace     = k3d_namespace.argocd.name
  name          = "guestbook"
  wait_for_sync = true
  timeout       = "6m"

  source = {
    repo_url        = %q
    target_revision = %q
    path            = "guestbook"
  }
  destination = {
    namespace = "guestbook"
  }
  sync_policy = {
    automated    = true
    prune        = true
    self_heal    = true
    sync_options = [%s]
  }

  depends_on = [k3d_helm_release.argocd]
}
`, cluster, testGuestbookRepo, testGuestbookCommit, options)
	}
	config := func(options string) string { return platform + application(options) }

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(`"CreateNamespace=true"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "sync_status", "Synced"),
				resource.TestCheckResourceAttr(address, "health_status", "Healthy"),
				testAccExpectKubectl(t, cluster, "guestbook-ui", "get", "deployment", "guestbook-ui", "-n", "guestbook", "-o", "jsonpath={.metadata.name}"),
			),
		},
		resource.TestStep{
			Config:           config(`"CreateNamespace=true", "ApplyOutOfSyncOnly=true"`),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check:            testAccExpectKubectl(t, cluster, "ApplyOutOfSyncOnly=true", "get", "application", "guestbook", "-n", "argocd", "-o", "jsonpath={.spec.syncPolicy.syncOptions[1]}"),
		},
		resource.TestStep{
			ResourceName:            address,
			ImportState:             true,
			ImportStateId:           cluster + "/argocd/guestbook",
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"source", "source.%", "source.path", "source.repo_url", "source.target_revision", "destination", "destination.%", "destination.namespace", "sync_policy", "sync_policy.%", "sync_policy.automated", "sync_policy.prune", "sync_policy.self_heal", "sync_policy.sync_options", "sync_policy.sync_options.#", "sync_policy.sync_options.0", "sync_policy.sync_options.1", "project", "cascade_delete", "wait_for_sync", "timeout"},
		},
		resource.TestStep{
			Config: platform,
			Check: testAccCheck(func() error {
				if out, err := testAccKubectlRaw(t, cluster, "get", "deployment", "guestbook-ui", "-n", "guestbook"); err == nil {
					return fmt.Errorf("cascade delete left the deployment: %s", out)
				}
				return nil
			}),
		},
	))
}

func TestAccArgoApplication_validation(t *testing.T) {
	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: `
resource "k3d_argocd_application" "test" {
  cluster = "tfacc-unused"
  name    = "a"
  source = {
    repo_url        = "https://r"
    target_revision = "abc"
  }
  destination = {
    namespace = "n"
  }
}
`,
		PlanOnly:    true,
		ExpectError: regexp.MustCompile(`Set exactly one of path or chart`),
	}))
}
