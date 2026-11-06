package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/helm"
	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/registry"
)

const (
	publicRepository   = "https://stefanprodan.github.io/podinfo"
	publicChart        = "podinfo"
	publicChartVersion = "6.16.0"
)

// testAccPushChart packages a local chart and pushes it to a registry, retrying
// while the registry container starts.
func testAccPushChart(t *testing.T, chartPath, reference string) {
	t.Helper()

	loaded, err := helm.Source{Path: chartPath}.Load()
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := loaded.(*chartv2.Chart)
	if !ok {
		t.Fatalf("unexpected chart type %T", loaded)
	}
	archive, err := chartutil.Save(ch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}

	client, err := registry.NewClient(registry.ClientOptPlainHTTP(), registry.ClientOptWriter(os.Stderr))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 30 {
		_, err = client.Push(data, reference)
		if err == nil {
			return
		}
		if attempt < 29 {
			time.Sleep(time.Second)
		}
	}
	t.Fatalf("push chart: %v", err)
}

// TestAccHelmRelease_oci installs from a real k3d_registry, so it needs no
// internet access beyond pulling the registry image.
func TestAccHelmRelease_oci(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	registryName := testAccObjectName()
	port := testAccFreePort(t)
	address := "k3d_helm_release.test"

	_, err := k3dclient.CreateRegistry(context.Background(), k3dclient.RegistrySpec{
		Name:    registryName,
		Image:   k3dclient.DefaultRegistryImage,
		HostIP:  "0.0.0.0",
		Port:    port,
		Network: k3dclient.DefaultRegistryNetwork,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k3dclient.DeleteRegistry(context.Background(), registryName); err != nil {
			t.Errorf("delete registry: %v", err)
		}
	})

	repository := fmt.Sprintf("localhost:%d/charts/demo", port)
	testAccPushChart(t, testAccChart(t), repository+":1.0.0")

	config := func(message string) string {
		return fmt.Sprintf(`
resource "k3d_helm_release" "test" {
  cluster       = %q
  name          = %q
  chart         = "oci://%s"
  chart_version = "1.0.0"
  plain_http    = true
  timeout       = "4m"
  values        = [yamlencode({ message = %q })]
}
`, cluster, name, repository, message)
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config("one"),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "deployed_chart_version", "1.0.0"),
				resource.TestCheckNoResourceAttr(address, "chart_digest"),
				testAccMessage(t, cluster, name, "one"),
			),
		},
		resource.TestStep{
			Config:           config("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check:            testAccMessage(t, cluster, name, "two"),
		},
	))
}

// TestAccHelmRelease_repository is the only test that needs a public chart.
func TestAccHelmRelease_repository(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()

	config := fmt.Sprintf(`
resource "k3d_helm_release" "test" {
  cluster       = %q
  name          = %q
  repository    = %q
  chart         = %q
  chart_version = %q
  wait          = false
}
`, cluster, name, publicRepository, publicChart, publicChartVersion)

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: config,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("k3d_helm_release.test", "deployed_chart_version", publicChartVersion),
			resource.TestCheckResourceAttr("k3d_helm_release.test", "status", "deployed"),
		),
	}))
}

// TestAccHelmRelease_writeOnlyValues proves write-only values reach the release
// but not Terraform state, and only a version bump applies a new value.
func TestAccHelmRelease_writeOnlyValues(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	chartPath := testAccChart(t)
	address := "k3d_helm_release.test"
	const (
		first  = "first-write-only-value"
		second = "second-write-only-value"
	)

	config := func(message string, version int) string {
		return testAccLocalRelease(cluster, name, chartPath, fmt.Sprintf(`
  values            = [yamlencode({ image = %q })]
  values_wo         = [yamlencode({ message = %q })]
  values_wo_version = %d
`, testJobImage, message, version))
	}
	notInState := func(value string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			for key, got := range s.RootModule().Resources[address].Primary.Attributes {
				if strings.Contains(got, value) {
					return fmt.Errorf("plaintext found in state attribute %s", key)
				}
			}
			return nil
		}
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(first, 1),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccMessage(t, cluster, name, first),
				notInState(first),
				resource.TestCheckResourceAttr(address, "revision", "1"),
			),
		},
		resource.TestStep{Config: config(second, 1), PlanOnly: true},
		resource.TestStep{
			Config:           config(second, 2),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccMessage(t, cluster, name, second),
				notInState(second),
				resource.TestCheckResourceAttr(address, "revision", "2"),
			),
		},
	))
}

func TestAccHelmRelease_kubeconfig(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	kubeconfig := testAccKubeconfigFile(t, cluster)
	chartPath := testAccChart(t)

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
resource "k3d_helm_release" "test" {
  kubeconfig = file(%q)
  name       = %q
  src_path   = %q
  timeout    = "4m"
}
`, kubeconfig, name, chartPath),
		Check: testAccMessage(t, cluster, name, "from-chart"),
	}))
}

// TestAccHelmRelease_takeOwnership covers a chart that templates its own
// namespace while a secret must already exist in it. Without take_ownership Helm
// refuses to adopt the namespace; with it, the release installs and the whole
// configuration still destroys cleanly.
func TestAccHelmRelease_takeOwnership(t *testing.T) {
	cluster := testAccSharedCluster(t)
	namespace := testAccObjectName()
	chartPath := testAccChart(t)
	testAccWriteFile(t, chartPath, "templates/namespace.yaml", `
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Release.Namespace }}
`)

	config := func(takeOwnership bool) string {
		return fmt.Sprintf(`
resource "k3d_namespace" "app" {
  cluster = %q
  name    = %q
}

resource "k3d_secret" "first" {
  cluster   = %q
  namespace = k3d_namespace.app.name
  name      = "must-exist-first"
  data      = { token = "abc" }
}

resource "k3d_helm_release" "test" {
  cluster        = %q
  namespace      = k3d_namespace.app.name
  name           = "demo"
  src_path       = %q
  timeout        = "4m"
  take_ownership = %t

  depends_on = [k3d_secret.first]
}
`, cluster, namespace, cluster, cluster, chartPath, takeOwnership)
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config:      config(false),
			ExpectError: regexp.MustCompile(`(?s)Failed to install the release.*ownership metadata`),
		},
		resource.TestStep{
			Config: config(true),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("k3d_helm_release.test", "status", "deployed"),
				testAccExpectKubectl(t, cluster, "from-chart", "get", "configmap", "demo", "-n", namespace, "-o", "jsonpath={.data.message}"),
			),
		},
	))
}

func TestAccHelmRelease_failures(t *testing.T) {
	cluster := testAccSharedCluster(t)
	chartPath := testAccChart(t)
	address := "k3d_helm_release.test"
	const badImage = "tfacc.invalid/missing:1"

	t.Run("failed install taints and retries", func(t *testing.T) {
		name := testAccObjectName()
		bad := testAccLocalRelease(cluster, name, chartPath, fmt.Sprintf(`
  timeout = "20s"
  values  = [yamlencode({ image = %q })]
`, badImage))
		bad = strings.Replace(bad, "  timeout  = \"4m\"\n", "", 1)
		good := testAccLocalRelease(cluster, name, chartPath, "")

		resource.Test(t, testAccCaseFor(
			resource.TestStep{Config: bad, ExpectError: regexp.MustCompile(`Failed to install the release`)},
			resource.TestStep{
				Config:           good,
				ConfigPlanChecks: expectAction(address, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(address, "status", "deployed"),
			},
		))
	})

	t.Run("atomic install leaves nothing", func(t *testing.T) {
		name := testAccObjectName()
		bad := testAccLocalRelease(cluster, name, chartPath, fmt.Sprintf(`
  atomic  = true
  timeout = "20s"
  values  = [yamlencode({ image = %q })]
`, badImage))
		bad = strings.Replace(bad, "  timeout  = \"4m\"\n", "", 1)

		resource.Test(t, testAccCaseFor(
			resource.TestStep{Config: bad, ExpectError: regexp.MustCompile(`Failed to install the release`)},
			resource.TestStep{
				Config:           testAccLocalRelease(cluster, name, chartPath, ""),
				ConfigPlanChecks: expectAction(address, plancheck.ResourceActionCreate),
			},
		))
	})

	t.Run("failed upgrade plans another upgrade", func(t *testing.T) {
		name := testAccObjectName()
		good := testAccLocalRelease(cluster, name, chartPath, "")
		bad := testAccLocalRelease(cluster, name, chartPath, fmt.Sprintf(`
  timeout = "20s"
  values  = [yamlencode({ image = %q })]
`, badImage))
		bad = strings.Replace(bad, "  timeout  = \"4m\"\n", "", 1)

		resource.Test(t, testAccCaseFor(
			resource.TestStep{Config: good},
			resource.TestStep{Config: bad, ExpectError: regexp.MustCompile(`Failed to upgrade the release`)},
			resource.TestStep{
				Config:           good,
				ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckResourceAttr(address, "status", "deployed"),
			},
		))
	})
}

// TestAccHelmRelease_oneApplyAndClusterReplace proves a release can share an
// apply with its cluster and namespace, and is recreated with the cluster.
func TestAccHelmRelease_oneApplyAndClusterReplace(t *testing.T) {
	name := testAccName(t)
	chartPath := testAccChart(t)

	config := func(agents int) string {
		return fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  agents  = %d
  traefik = false
}

resource "k3d_namespace" "app" {
  cluster = k3d_cluster.test.id
  name    = "app"
}

resource "k3d_helm_release" "test" {
  cluster   = k3d_cluster.test.id
  namespace = k3d_namespace.app.name
  name      = "demo"
  src_path  = %q
  timeout   = "4m"
}
`, name, testImage, agents, chartPath)
	}
	inNamespace := testAccExpectKubectl(t, name, "from-chart", "get", "configmap", "demo", "-n", "app", "-o", "jsonpath={.data.message}")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{Config: config(0), Check: inNamespace},
			{
				Config: config(1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("k3d_cluster.test", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_helm_release.test", plancheck.ResourceActionReplace),
					},
				},
				Check: inNamespace,
			},
		},
	})
}
