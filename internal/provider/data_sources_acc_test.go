package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccDataSources_kubeObjects(t *testing.T) {
	cluster := testAccSharedCluster(t)
	namespace := testAccObjectName()

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
locals {
  cluster = %q
}

resource "k3d_namespace" "test" {
  cluster     = local.cluster
  name        = %q
  labels      = { team = "a" }
  annotations = { note = "b" }
}

resource "k3d_config_map" "test" {
  cluster   = local.cluster
  namespace = k3d_namespace.test.name
  name      = "settings"
  data      = { mode = "test" }
}

resource "k3d_secret" "test" {
  cluster   = local.cluster
  namespace = k3d_namespace.test.name
  name      = "credentials"
  data      = { token = "abc" }
}

data "k3d_namespace" "test" {
  cluster = local.cluster
  name    = k3d_namespace.test.name
}

data "k3d_config_map" "test" {
  cluster   = local.cluster
  namespace = k3d_config_map.test.namespace
  name      = k3d_config_map.test.name
}

data "k3d_secret" "test" {
  cluster   = local.cluster
  namespace = k3d_secret.test.namespace
  name      = k3d_secret.test.name
}

data "k3d_config_map" "defaulted" {
  cluster = local.cluster
  name    = "kube-root-ca.crt"
}

data "k3d_context" "test" {
  cluster = local.cluster
}
`, cluster, namespace),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.k3d_namespace.test", "labels.team", "a"),
			resource.TestCheckResourceAttr("data.k3d_namespace.test", "labels.kubernetes.io/metadata.name", namespace),
			resource.TestCheckResourceAttr("data.k3d_namespace.test", "annotations.note", "b"),
			resource.TestCheckResourceAttr("data.k3d_namespace.test", "phase", "Active"),
			resource.TestCheckResourceAttrSet("data.k3d_namespace.test", "id"),
			resource.TestCheckResourceAttr("data.k3d_config_map.test", "data.mode", "test"),
			resource.TestCheckResourceAttr("data.k3d_config_map.defaulted", "namespace", "default"),
			resource.TestCheckResourceAttrSet("data.k3d_config_map.defaulted", "data.ca.crt"),
			resource.TestCheckResourceAttr("data.k3d_secret.test", "data.token", "abc"),
			resource.TestCheckResourceAttr("data.k3d_secret.test", "type", "Opaque"),
			resource.TestCheckResourceAttr("data.k3d_context.test", "name", "k3d-"+cluster),
			resource.TestCheckResourceAttr("data.k3d_context.test", "cluster_name", "k3d-"+cluster),
			resource.TestCheckResourceAttr("data.k3d_context.test", "user_name", "admin@k3d-"+cluster),
			resource.TestMatchResourceAttr("data.k3d_context.test", "host", regexp.MustCompile(`^https://`)),
		),
	}))
}

func TestAccDataSources_contextFromKubeconfig(t *testing.T) {
	cluster := testAccSharedCluster(t)
	kubeconfig := testAccKubeconfigFile(t, cluster)

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
data "k3d_context" "test" {
  kubeconfig = file(%q)
}
`, kubeconfig),
		Check: resource.TestCheckResourceAttr("data.k3d_context.test", "name", "k3d-"+cluster),
	}))
}

func TestAccDataSources_missingObjectIsAnError(t *testing.T) {
	cluster := testAccSharedCluster(t)

	tests := map[string]string{
		"namespace":  fmt.Sprintf("data \"k3d_namespace\" \"t\" {\n  cluster = %q\n  name = \"tfacc-missing\"\n}\n", cluster),
		"config map": fmt.Sprintf("data \"k3d_config_map\" \"t\" {\n  cluster = %q\n  name = \"tfacc-missing\"\n}\n", cluster),
		"secret":     fmt.Sprintf("data \"k3d_secret\" \"t\" {\n  cluster = %q\n  name = \"tfacc-missing\"\n}\n", cluster),
		"no cluster": "data \"k3d_context\" \"t\" {\n  cluster = \"tfacc-no-such-cluster\"\n}\n",
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, testAccCaseFor(resource.TestStep{
				Config:      cfg,
				ExpectError: regexp.MustCompile(`Failed to read|not found`),
			}))
		})
	}
}

func TestAccDataSources_needExactlyOneConnection(t *testing.T) {
	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config:      "data \"k3d_namespace\" \"t\" {\n  name = \"a\"\n}\n",
		ExpectError: regexp.MustCompile(`Exactly one of these attributes must be configured: \[cluster,kubeconfig\]`),
	}))
}

// TestAccExample_minimal applies the example exactly as shipped, so it cannot
// drift from what the provider accepts. The test framework writes its own
// required_providers, which is why versions.tf is left out.
func TestAccExample_minimal(t *testing.T) {
	testAccRequire(t)
	name := testAccName(t)
	port := testAccFreePort(t)

	var source strings.Builder
	for _, file := range []string{"main.tf", "variables.tf"} {
		content, err := os.ReadFile(filepath.Join("..", "..", "examples", "minimal", file))
		if err != nil {
			t.Fatal(err)
		}
		source.Write(content)
		source.WriteString("\n")
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{{
			Config: source.String(),
			ConfigVariables: config.Variables{
				"cluster_name": config.StringVariable(name),
				"http_port":    config.IntegerVariable(port),
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccKubeconfigWorks(t, "k3d_cluster.main", 2),
				testAccHTTPReachable(port),
				resource.TestCheckOutput("context", "k3d-"+name),
				resource.TestCheckOutput("url", fmt.Sprintf("http://localhost:%d", port)),
			),
		}},
	})
}
