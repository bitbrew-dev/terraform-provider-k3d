package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	testIstioVersion    = "1.30.1"
	testIstioUpgrade    = "1.30.2"
	testGatewayNodePort = 31080
	testAppImage        = "nginx:1.27-alpine"
)

func testAccIstioCluster(name string, hostPort int) string {
	return fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false
  ports   = [{ host_port = %d, container_port = %d }]
}

resource "k3d_namespace" "mesh" {
  cluster = k3d_cluster.test.id
  name    = "istio-system"
}
`, name, testImage, hostPort, testGatewayNodePort)
}

func testAccIstioMesh(version string) string {
	return fmt.Sprintf(`
resource "k3d_istio" "mesh" {
  cluster   = k3d_cluster.test.id
  namespace = k3d_namespace.mesh.name
  version   = %q
  timeout   = "3m"

  gateway = {
    http_node_port = %d
  }
}
`, version, testGatewayNodePort)
}

// testAccIstioApp is a workload behind the mesh, created in the same apply as
// Istio, so its Gateway and VirtualService go through Istio's webhook while it
// has only just started.
func testAccIstioApp() string {
	return fmt.Sprintf(`
resource "k3d_namespace" "app" {
  cluster = k3d_cluster.test.id
  name    = "app"
  labels  = { "istio-injection" = "enabled" }

  depends_on = [k3d_istio.mesh]
}

resource "k3d_manifest" "deployment" {
  cluster = k3d_cluster.test.id
  wait    = true
  manifest = yamlencode({
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = "web", namespace = k3d_namespace.app.name }
    spec = {
      selector = { matchLabels = { app = "web" } }
      template = {
        metadata = { labels = { app = "web" } }
        spec     = { containers = [{ name = "main", image = %q, ports = [{ containerPort = 80 }] }] }
      }
    }
  })
}

resource "k3d_manifest" "service" {
  cluster = k3d_cluster.test.id
  manifest = yamlencode({
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "web", namespace = k3d_namespace.app.name }
    spec       = { selector = { app = "web" }, ports = [{ name = "http", port = 80 }] }
  })
}

resource "k3d_manifest" "gateway" {
  cluster = k3d_cluster.test.id
  manifest = yamlencode({
    apiVersion = "networking.istio.io/v1"
    kind       = "Gateway"
    metadata   = { name = "web", namespace = k3d_namespace.app.name }
    spec = {
      selector = { istio = "ingressgateway" }
      servers  = [{ port = { number = 80, name = "http", protocol = "HTTP" }, hosts = ["*"] }]
    }
  })
}

resource "k3d_manifest" "route" {
  cluster = k3d_cluster.test.id
  manifest = yamlencode({
    apiVersion = "networking.istio.io/v1"
    kind       = "VirtualService"
    metadata   = { name = "web", namespace = k3d_namespace.app.name }
    spec = {
      hosts    = ["*"]
      gateways = ["web"]
      http     = [{ route = [{ destination = { host = "web" } }] }]
    }
  })
}
`, testAppImage)
}

// testAccHTTPReachable follows the real path: host port, k3d load balancer,
// gateway NodePort, gateway pod, sidecar, and the app.
func testAccHTTPReachable(hostPort int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		url := fmt.Sprintf("http://localhost:%d/", hostPort)
		client := &http.Client{Timeout: 5 * time.Second}

		var last string
		for range 60 {
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err == nil {
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK && strings.Contains(string(body), "nginx") {
					return nil
				}
				last = fmt.Sprintf("status %d", response.StatusCode)
			} else {
				last = err.Error()
			}
			time.Sleep(2 * time.Second)
		}
		return fmt.Errorf("%s never served the app: %s", url, last)
	}
}

func TestAccIstio_lifecycle(t *testing.T) {
	name := testAccName(t)
	hostPort := testAccFreePort(t)
	address := "k3d_istio.mesh"
	cluster := testAccIstioCluster(name, hostPort)

	istiodImage := func(version string) resource.TestCheckFunc {
		return testAccCheck(func() error {
			image := testAccKubectl(t, name, "get", "deployment", "istiod", "-n", "istio-system", "-o", "jsonpath={.spec.template.spec.containers[0].image}")
			if !strings.HasSuffix(image, ":"+version) {
				return fmt.Errorf("istiod image = %s, want version %s", image, version)
			}
			return nil
		})
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{
				Config: cluster + testAccIstioMesh(testIstioVersion) + testAccIstioApp(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "deployed"),
					resource.TestCheckResourceAttr(address, "base_revision", "1"),
					resource.TestCheckResourceAttr(address, "istiod_revision", "1"),
					resource.TestCheckResourceAttr(address, "gateway_revision", "1"),
					istiodImage(testIstioVersion),
					testAccExpectKubectl(t, name, "NodePort", "get", "service", defaultGatewayName, "-n", "istio-system", "-o", "jsonpath={.spec.type}"),
					testAccExpectKubectl(t, name, "main istio-proxy", "get", "pods", "-n", "app", "-l", "app=web", "-o", "jsonpath={.items[0].spec.containers[*].name}"),
					testAccHTTPReachable(hostPort),
				),
			},
			{
				Config:           cluster + testAccIstioMesh(testIstioUpgrade) + testAccIstioApp(),
				ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "istiod_revision", "2"),
					istiodImage(testIstioUpgrade),
					testAccHTTPReachable(hostPort),
				),
			},
			{
				PreConfig: func() {
					if err := testAccHelmNamespaceClient(t, name, "istio-system").Uninstall(defaultGatewayName, true, 3*time.Minute); err != nil {
						t.Fatal(err)
					}
				},
				Config:           cluster + testAccIstioMesh(testIstioUpgrade) + testAccIstioApp(),
				ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "deployed"),
					testAccHTTPReachable(hostPort),
				),
			},
			{
				Config: cluster,
				Check: testAccCheck(func() error {
					if out, err := testAccKubectlRaw(t, name, "get", "deployment", "istiod", "-n", "istio-system"); err == nil {
						return fmt.Errorf("istiod still exists: %s", out)
					}
					return nil
				}),
			},
			{
				Config: cluster + testAccIstioMesh(testIstioVersion),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "status", "deployed"),
					istiodImage(testIstioVersion),
				),
			},
			{
				ResourceName:            address,
				ImportState:             true,
				ImportStateId:           name + "/istio-system",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"version", "gateway", "gateway.%", "gateway.http_node_port", "gateway_revision", "repository", "take_ownership", "timeout"},
			},
		},
	})
}
