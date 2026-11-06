package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// testAccChart writes a chart with a ConfigMap showing its values, a Deployment
// that goes ready on its own, and a post-install hook.
func testAccChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testAccWriteFile(t, dir, "Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.0.0\nappVersion: \"2.0\"\n")
	testAccWriteFile(t, dir, "values.yaml", "message: from-chart\nimage: "+testJobImage+"\n")
	testAccWriteFile(t, dir, ".helmignore", "*.md\n")
	testAccWriteFile(t, dir, "templates/configmap.yaml", `
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}
data:
  message: {{ .Values.message | quote }}
`)
	testAccWriteFile(t, dir, "templates/deployment.yaml", `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  selector:
    matchLabels:
      app: {{ .Release.Name }}
  template:
    metadata:
      labels:
        app: {{ .Release.Name }}
    spec:
      terminationGracePeriodSeconds: 0
      containers:
        - name: main
          image: {{ .Values.image }}
          command: ["sleep", "3600"]
`)
	testAccWriteFile(t, dir, "templates/hook.yaml", `
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .Release.Name }}-hook
  annotations:
    helm.sh/hook: post-install
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: main
          image: {{ .Values.image }}
          command: ["true"]
`)
	return dir
}

func testAccWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testAccHelmClient(t *testing.T, cluster string) *helm.Client {
	t.Helper()
	raw, err := k3dclient.GetKubeconfig(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	client, err := helm.New(raw, "default")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testAccInstallOutside(t *testing.T, cluster, chartPath, name string, values map[string]any) {
	t.Helper()
	ch, err := helm.Source{Path: chartPath}.Load()
	if err != nil {
		t.Fatal(err)
	}
	options := helm.Options{Name: name, Namespace: "default", Wait: true, MaxHistory: 10, Timeout: 4 * time.Minute}
	if _, err := testAccHelmClient(t, cluster).Install(context.Background(), ch, values, options); err != nil {
		t.Fatal(err)
	}
}

func testAccMessage(t *testing.T, cluster, release, want string) resource.TestCheckFunc {
	return testAccExpectKubectl(t, cluster, want, "get", "configmap", release, "-n", "default", "-o", "jsonpath={.data.message}")
}

func testAccLocalRelease(cluster, name, chartPath, extra string) string {
	return fmt.Sprintf(`
resource "k3d_helm_release" "test" {
  cluster  = %q
  name     = %q
  src_path = %q
  timeout  = "4m"
  %s
}
`, cluster, name, chartPath, extra)
}

func TestAccHelmRelease_localLifecycle(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	chartPath := testAccChart(t)
	address := "k3d_helm_release.test"

	config := func(extra string) string { return testAccLocalRelease(cluster, name, chartPath, extra) }
	withMessage := func(message string) string {
		return config(fmt.Sprintf("values = [yamlencode({ message = %q })]", message))
	}

	var firstDigest string
	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: withMessage("one"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "revision", "1"),
				resource.TestCheckResourceAttr(address, "status", "deployed"),
				resource.TestCheckResourceAttr(address, "deployed_chart_version", "1.0.0"),
				resource.TestCheckResourceAttr(address, "app_version", "2.0"),
				resource.TestCheckResourceAttr(address, "namespace", "default"),
				resource.TestCheckResourceAttr(address, "id", cluster+"/default/"+name),
				testAccMessage(t, cluster, name, "one"),
				testAccExpectKubectl(t, cluster, "True", "get", "job", name+"-hook", "-n", "default", "-o", `jsonpath={.status.conditions[?(@.type=="Complete")].status}`),
				func(s *terraform.State) error {
					firstDigest = s.RootModule().Resources[address].Primary.Attributes["chart_digest"]
					if firstDigest == "" {
						return fmt.Errorf("chart_digest is empty")
					}
					return nil
				},
			),
		},
		resource.TestStep{
			Config:           withMessage("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "revision", "2"),
				testAccMessage(t, cluster, name, "two"),
			),
		},
		resource.TestStep{
			Config:    withMessage("two") + "\n",
			PreConfig: func() { testAccWriteFile(t, chartPath, "NOTES.md", "ignored by .helmignore") },
			PlanOnly:  true,
		},
		resource.TestStep{
			PreConfig: func() {
				testAccWriteFile(t, chartPath, "templates/extra.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-extra\n")
			},
			Config:           withMessage("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "revision", "3"),
				testAccExpectKubectl(t, cluster, name+"-extra", "get", "configmap", name+"-extra", "-n", "default", "-o", "jsonpath={.metadata.name}"),
				func(s *terraform.State) error {
					if s.RootModule().Resources[address].Primary.Attributes["chart_digest"] == firstDigest {
						return fmt.Errorf("chart_digest did not change")
					}
					return nil
				},
			),
		},
		resource.TestStep{
			Config: config(`
  values = [yamlencode({ message = "from-values" })]
  set    = { message = "from-set" }
`),
			Check: testAccMessage(t, cluster, name, "from-set"),
		},
		resource.TestStep{
			Config:           config(""),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check:            testAccMessage(t, cluster, name, "from-chart"),
		},
	))

	if out, err := testAccKubectlRaw(t, cluster, "get", "configmap", name, "-n", "default"); err == nil {
		t.Errorf("configmap still exists after destroy: %s", out)
	}
}

func TestAccHelmRelease_driftIsRestored(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	chartPath := testAccChart(t)
	address := "k3d_helm_release.test"
	config := testAccLocalRelease(cluster, name, chartPath, `values = [yamlencode({ message = "managed" })]`)

	upgradeOutside := func() {
		ch, err := helm.Source{Path: chartPath}.Load()
		if err != nil {
			t.Fatal(err)
		}
		options := helm.Options{Name: name, Namespace: "default", Timeout: 4 * time.Minute}
		if _, err := testAccHelmClient(t, cluster).Upgrade(context.Background(), ch, map[string]any{"message": "changed-by-hand"}, options); err != nil {
			t.Fatal(err)
		}
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{Config: config},
		resource.TestStep{
			PreConfig:        upgradeOutside,
			Config:           config,
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccMessage(t, cluster, name, "managed"),
				resource.TestCheckResourceAttr(address, "revision", "3"),
			),
		},
		resource.TestStep{Config: config, PlanOnly: true},
		resource.TestStep{
			PreConfig: func() {
				if err := testAccHelmClient(t, cluster).Uninstall(name, false, time.Minute); err != nil {
					t.Fatal(err)
				}
				testAccKubectl(t, cluster, "delete", "job", name+"-hook", "-n", "default", "--wait=true", "--ignore-not-found")
			},
			Config:           config,
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionCreate),
			Check:            testAccMessage(t, cluster, name, "managed"),
		},
	))
}

func TestAccHelmRelease_importAdoptsWithoutReplacing(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	chartPath := testAccChart(t)
	address := "k3d_helm_release.test"
	config := testAccLocalRelease(cluster, name, chartPath, `values = [yamlencode({ message = "managed" })]`)

	testAccInstallOutside(t, cluster, chartPath, name, map[string]any{"message": "outside"})
	uid := testAccKubectl(t, cluster, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}")

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config:      config,
			ExpectError: regexp.MustCompile(`already exists: import it`),
		},
		resource.TestStep{
			Config:             config,
			ResourceName:       address,
			ImportState:        true,
			ImportStateId:      cluster + "/default/" + name,
			ImportStatePersist: true,
		},
		resource.TestStep{
			Config:           config,
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccMessage(t, cluster, name, "managed"),
				resource.TestCheckResourceAttr(address, "revision", "2"),
				testAccExpectKubectl(t, cluster, uid, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}"),
			),
		},
	))
}

func TestAccHelmRelease_planTimeChecks(t *testing.T) {
	cluster := testAccSharedCluster(t)
	chartPath := testAccChart(t)
	name := testAccObjectName()

	dependencyChart := t.TempDir()
	testAccWriteFile(t, dependencyChart, "Chart.yaml", "apiVersion: v2\nname: demo\nversion: 1.0.0\ndependencies:\n- name: sub\n  version: 1.0.0\n  repository: https://example.com\n")

	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "wrong digest",
			config:  testAccLocalRelease(cluster, name, chartPath, `expected_chart_digest = "wrong"`),
			wantErr: `Chart digest mismatch`,
		},
		{
			name:    "missing path",
			config:  testAccLocalRelease(cluster, name, filepath.Join(chartPath, "missing"), ""),
			wantErr: `Invalid chart`,
		},
		{
			name:    "dependencies without a lock",
			config:  testAccLocalRelease(cluster, name, dependencyChart, ""),
			wantErr: `no Chart.lock`,
		},
		{
			name:    "both sources",
			config:  testAccLocalRelease(cluster, name, chartPath, `chart = "demo"`),
			wantErr: `Exactly one of these attributes must be configured: \[src_path,chart\]`,
		},
		{
			name:    "remote without a version",
			config:  fmt.Sprintf("resource \"k3d_helm_release\" \"test\" {\n  cluster = %q\n  name = %q\n  chart = \"oci://example.com/c\"\n}\n", cluster, name),
			wantErr: `chart_version is required`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource.Test(t, testAccCaseFor(resource.TestStep{
				Config:      tt.config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(tt.wantErr),
			}))
		})
	}
}
