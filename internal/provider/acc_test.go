package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

const testImage = "rancher/k3s:v1.32.4-k3s1"

var testAccFactories = map[string]func() (tfprotov6.ProviderServer, error){
	typeName: providerserver.NewProtocol6WithError(New("test")()),
}

var testAccVersionChecks = []tfversion.TerraformVersionCheck{
	tfversion.SkipBelow(tfversion.Version1_11_0),
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Fatalf("acceptance tests need a running Docker: %v", err)
	}
}

// testAccRequire skips a test that prepares Docker objects before resource.Test,
// which would otherwise create them during a plain unit test run.
func testAccRequire(t *testing.T) {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skipf("set %s to run acceptance tests", resource.EnvTfAcc)
	}
	testAccPreCheck(t)
}

func testAccName(t *testing.T) string {
	t.Helper()
	return "tfacc-" + acctest.RandStringFromCharSet(8, acctest.CharSetAlphaNum)
}

// testAccFreePort returns a port that is free now, so tests never rely on a
// fixed host port.
func testAccFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

func testAccDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func testAccClusterGone(name string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		_, err := k3dclient.GetCluster(context.Background(), name)
		if err == nil {
			return fmt.Errorf("cluster %s still exists", name)
		}
		if !errors.Is(err, k3dclient.ErrNotFound) {
			return err
		}
		if testAccNetworkExists("k3d-" + name) {
			return fmt.Errorf("network k3d-%s was left behind", name)
		}
		return nil
	}
}

func testAccNetworkExists(name string) bool {
	return exec.Command("docker", "network", "inspect", name).Run() == nil
}

// testAccKubeconfigWorks reads nodes through the exported kubeconfig, passed
// explicitly so the user's default kubeconfig is never involved.
func testAccKubeconfigWorks(t *testing.T, address string, wantNodes int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		res, ok := s.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("%s not in state", address)
		}
		path := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(path, []byte(res.Primary.Attributes["kubeconfig"]), 0o600); err != nil {
			return err
		}
		var (
			out []byte
			err error
		)
		for range 30 {
			out, err = exec.Command("kubectl", "--kubeconfig", path, "get", "nodes", "--no-headers", "--request-timeout=10s").CombinedOutput()
			if err == nil && strings.Count(strings.TrimSpace(string(out)), "\n")+1 == wantNodes {
				return nil
			}
			time.Sleep(2 * time.Second)
		}
		return fmt.Errorf("want %d nodes through the kubeconfig (err %v):\n%s", wantNodes, err, out)
	}
}

func testAccCheck(fn func() error) resource.TestCheckFunc {
	return func(*terraform.State) error { return fn() }
}

func TestAccProvider_dockerHost(t *testing.T) {
	t.Setenv(dockerHostEnv, os.Getenv(dockerHostEnv))

	const dataSource = `
data "k3d_cluster" "test" {
  name = "tfacc-does-not-exist"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps: []resource.TestStep{
			{
				Config:      `provider "k3d" { docker_host = "not-a-docker-host" }` + dataSource,
				ExpectError: regexp.MustCompile(`must start with unix://, tcp://, or ssh://`),
			},
			{
				Config:      `provider "k3d" { docker_host = "tcp://127.0.0.1:1" }` + dataSource,
				ExpectError: regexp.MustCompile(`(?s)list nodes of cluster.*connect`),
			},
		},
	})
}

func TestAccCluster_lifecycle(t *testing.T) {
	name := testAccName(t)
	address := "k3d_cluster.test"
	portA, portB := testAccFreePort(t), testAccFreePort(t)

	config := func(ports string) string {
		return fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false
  ports   = [%s]
}
`, name, testImage, ports)
	}
	onePort := fmt.Sprintf(`{ host_port = %d, container_port = 30080 }`, portA)
	twoPorts := onePort + fmt.Sprintf(`, { host_port = %d, container_port = 30081, protocol = "tcp" }`, portB)

	var serverID string
	serverContainer := "k3d-" + name + "-server-0"
	lbContainer := "k3d-" + name + "-serverlb"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{
				Config: config(onePort),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "id", name),
					resource.TestCheckResourceAttr(address, "image", testImage),
					resource.TestCheckResourceAttr(address, "servers", "1"),
					resource.TestCheckResourceAttr(address, "agents", "0"),
					resource.TestCheckResourceAttr(address, "network", "k3d-"+name),
					resource.TestCheckResourceAttrSet(address, "token"),
					resource.TestCheckResourceAttrSet(address, "kubeconfig"),
					resource.TestMatchResourceAttr(address, "host", regexp.MustCompile(`^https://`)),
					resource.TestMatchResourceAttr(address, "client_key", regexp.MustCompile(`PRIVATE KEY`)),
					resource.TestMatchResourceAttr(address, "cluster_ca_certificate", regexp.MustCompile(`CERTIFICATE`)),
					testAccKubeconfigWorks(t, address, 1),
					testAccCheck(func() error {
						serverID = testAccDocker(t, "inspect", "-f", "{{.Id}}", serverContainer)
						image := testAccDocker(t, "inspect", "-f", "{{.Config.Image}}", lbContainer)
						if !strings.HasSuffix(image, ":"+strings.TrimPrefix(k3dclient.LibraryVersion, "v")) {
							return fmt.Errorf("load balancer image %s is not pinned", image)
						}
						return nil
					}),
				),
			},
			{
				ResourceName:      address,
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"traefik", "timeout",
				},
			},
			{
				Config: config(twoPorts),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: testAccCheck(func() error {
					if id := testAccDocker(t, "inspect", "-f", "{{.Id}}", serverContainer); id != serverID {
						return fmt.Errorf("server container was replaced")
					}
					if out := testAccDocker(t, "port", lbContainer); !strings.Contains(out, fmt.Sprint(portB)) {
						return fmt.Errorf("port %d not published:\n%s", portB, out)
					}
					return nil
				}),
			},
			{
				Config: config(onePort),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: testAccCheck(func() error {
					if id := testAccDocker(t, "inspect", "-f", "{{.Id}}", serverContainer); id != serverID {
						return fmt.Errorf("server container was replaced")
					}
					out := testAccDocker(t, "port", lbContainer)
					if strings.Contains(out, fmt.Sprint(portB)) {
						return fmt.Errorf("port %d still published:\n%s", portB, out)
					}
					if !strings.Contains(out, fmt.Sprint(portA)) {
						return fmt.Errorf("port %d was lost:\n%s", portA, out)
					}
					return nil
				}),
			},
			{
				PreConfig: func() {
					testAccDocker(t, "stop", "-t", "1", lbContainer)
					testAccDocker(t, "start", lbContainer)
				},
				RefreshState: true,
			},
		},
	})
}

func TestAccCluster_options(t *testing.T) {
	name := testAccName(t)
	address := "k3d_cluster.test"
	source := t.TempDir()
	apiPort := testAccFreePort(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  agents  = 1
  traefik = false

  api = {
    port = %d
  }

  k3s_args = [
    { arg = "--tls-san=tfacc.example.com", node_filters = ["server:*"] },
  ]
  node_labels = [
    { key = "tfacc", value = "all" },
    { key = "tfacc-agent", value = "yes", node_filters = ["agent:0"] },
  ]
  runtime_labels = [
    { key = "tfacc.runtime", value = "yes" },
  ]
  env = [
    { name = "TFACC_ENV", value = "yes", node_filters = ["agent:*"] },
  ]
  volumes = [
    { source = %q, destination = "/tfacc" },
  ]
  host_aliases = [
    { ip = "10.1.2.3", hostnames = ["tfacc.internal"] },
  ]
  registries = {
    config = "mirrors:\n  docker.io:\n    endpoint:\n      - https://registry-1.docker.io"
  }
}

data "k3d_cluster" "test" {
  name = k3d_cluster.test.name
}
`, name, testImage, apiPort, source),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "agents", "1"),
					resource.TestCheckResourceAttr(address, "api.port", fmt.Sprint(apiPort)),
					resource.TestCheckResourceAttr("data.k3d_cluster.test", "agents", "1"),
					resource.TestCheckResourceAttrPair("data.k3d_cluster.test", "kubeconfig", address, "kubeconfig"),
					testAccCheck(func() error {
						server := "k3d-" + name + "-server-0"
						agent := "k3d-" + name + "-agent-0"

						serverArgs := testAccDocker(t, "inspect", "-f", "{{.Args}}", server)
						for _, want := range []string{"--disable=traefik", "--tls-san=tfacc.example.com", "--node-label tfacc=all"} {
							if !strings.Contains(serverArgs, want) {
								return fmt.Errorf("server args %s missing %s", serverArgs, want)
							}
						}
						agentArgs := testAccDocker(t, "inspect", "-f", "{{.Args}}", agent)
						if !strings.Contains(agentArgs, "--node-label tfacc-agent=yes") {
							return fmt.Errorf("agent args %s missing the agent label", agentArgs)
						}
						if strings.Contains(serverArgs, "tfacc-agent") {
							return fmt.Errorf("agent label reached the server: %s", serverArgs)
						}
						if env := testAccDocker(t, "inspect", "-f", "{{.Config.Env}}", agent); !strings.Contains(env, "TFACC_ENV=yes") {
							return fmt.Errorf("agent env missing TFACC_ENV")
						}
						if env := testAccDocker(t, "inspect", "-f", "{{.Config.Env}}", server); strings.Contains(env, "TFACC_ENV") {
							return fmt.Errorf("TFACC_ENV reached the server")
						}
						if labels := testAccDocker(t, "inspect", "-f", "{{index .Config.Labels \"tfacc.runtime\"}}", server); labels != "yes" {
							return fmt.Errorf("runtime label = %q", labels)
						}
						if mounts := testAccDocker(t, "inspect", "-f", "{{json .Mounts}}", agent); !strings.Contains(mounts, "/tfacc") {
							return fmt.Errorf("volume missing on agent: %s", mounts)
						}
						if reg := testAccDocker(t, "exec", server, "cat", "/etc/rancher/k3s/registries.yaml"); !strings.Contains(reg, "registry-1.docker.io") {
							return fmt.Errorf("registries.yaml = %s", reg)
						}
						if hosts := testAccDocker(t, "exec", server, "cat", "/etc/hosts"); !strings.Contains(hosts, "tfacc.internal") {
							return fmt.Errorf("/etc/hosts missing the alias:\n%s", hosts)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccCluster_multiServerPorts(t *testing.T) {
	name := testAccName(t)
	address := "k3d_cluster.test"
	portA, portB := testAccFreePort(t), testAccFreePort(t)

	config := func(ports string) string {
		return fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  servers = 3
  agents  = 1
  traefik = false
  ports   = [%s]
}
`, name, testImage, ports)
	}
	onePort := fmt.Sprintf(`{ host_port = %d, container_port = 30080 }`, portA)
	twoPorts := onePort + fmt.Sprintf(`, { host_port = %d, container_port = 30081 }`, portB)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{
				Config: config(onePort),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "servers", "3"),
					resource.TestCheckResourceAttr(address, "agents", "1"),
					testAccKubeconfigWorks(t, address, 4),
				),
			},
			{
				Config: config(twoPorts),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: testAccKubeconfigWorks(t, address, 4),
			},
		},
	})
}

func TestAccCluster_twoInOneConfig(t *testing.T) {
	nameA, nameB := testAccName(t), testAccName(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy: func(s *terraform.State) error {
			if err := testAccClusterGone(nameA)(s); err != nil {
				return err
			}
			return testAccClusterGone(nameB)(s)
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "k3d_cluster" "a" {
  name    = %q
  image   = %q
  traefik = false
}

resource "k3d_cluster" "b" {
  name    = %q
  image   = %q
  traefik = false
}
`, nameA, testImage, nameB, testImage),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccKubeconfigWorks(t, "k3d_cluster.a", 1),
					testAccKubeconfigWorks(t, "k3d_cluster.b", 1),
				),
			},
		},
	})
}

func TestAccCluster_externalNetworkIsKept(t *testing.T) {
	testAccRequire(t)
	name := testAccName(t)
	network := "tfacc-net-" + name

	testAccDocker(t, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy: func(*terraform.State) error {
			if !testAccNetworkExists(network) {
				return fmt.Errorf("external network %s was removed", network)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  network = %q
  traefik = false
}
`, name, testImage, network),
				Check: resource.TestCheckResourceAttr("k3d_cluster.test", "network", network),
			},
		},
	})
}

func TestAccCluster_replaceOnCreateOnlyChange(t *testing.T) {
	const newerImage = "rancher/k3s:v1.32.5-k3s1"

	name := testAccName(t)
	address := "k3d_cluster.test"

	config := func(image string, agents int) string {
		return fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  agents  = %d
  traefik = false
}
`, name, image, agents)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{Config: config(testImage, 0)},
			{
				Config: config(testImage, 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionReplace)},
				},
				Check: resource.TestCheckResourceAttr(address, "agents", "1"),
			},
			{
				Config: config(newerImage, 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionReplace)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(address, "image", newerImage),
					testAccCheck(func() error {
						image := testAccDocker(t, "inspect", "-f", "{{.Config.Image}}", "k3d-"+name+"-server-0")
						if image != newerImage {
							return fmt.Errorf("server runs %s, want %s", image, newerImage)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccCluster_recreatedAfterDeletedOutsideTerraform(t *testing.T) {
	name := testAccName(t)
	address := "k3d_cluster.test"
	config := fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false
}
`, name, testImage)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if err := k3dclient.DeleteCluster(context.Background(), name); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionCreate)},
				},
			},
		},
	})
}

// TestAccCluster_importAdoptsConfig proves importing a cluster and writing
// config for settings k3d cannot report does not destroy the cluster.
func TestAccCluster_importAdoptsConfig(t *testing.T) {
	testAccRequire(t)
	name := testAccName(t)
	address := "k3d_cluster.test"

	_, err := k3dclient.CreateCluster(context.Background(), k3dclient.ClusterSpec{
		Name:    name,
		Image:   testImage,
		Servers: 1,
		K3sArgs: []k3dclient.K3sArg{{Arg: "--tls-san=adopted.example.com"}},
		Timeout: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k3dclient.DeleteCluster(context.Background(), name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	serverContainer := "k3d-" + name + "-server-0"
	serverID := testAccDocker(t, "inspect", "-f", "{{.Id}}", serverContainer)

	config := fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false

  k3s_args = [
    { arg = "--tls-san=adopted.example.com" },
  ]
}
`, name, testImage)

	unchanged := testAccCheck(func() error {
		if id := testAccDocker(t, "inspect", "-f", "{{.Id}}", serverContainer); id != serverID {
			return fmt.Errorf("server container was replaced")
		}
		return nil
	})

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       address,
				ImportState:        true,
				ImportStateId:      name,
				ImportStatePersist: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)},
				},
				Check: unchanged,
			},
			{
				Config: strings.Replace(config, "adopted.example.com", "changed.example.com", 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionReplace)},
				},
			},
		},
	})
}

func TestAccCluster_failedCreateRollsBack(t *testing.T) {
	testAccRequire(t)
	name := testAccName(t)
	port := testAccFreePort(t)

	blocker := "tfacc-blocker-" + name
	testAccDocker(t, "run", "-d", "--name", blocker, "-p", fmt.Sprintf("%d:80", port), testImage, "server", "--disable-agent")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", blocker).Run() })

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false
  ports   = [{ host_port = %d, container_port = 30080 }]
}
`, name, testImage, port),
				ExpectError: regexp.MustCompile(`(?s)Failed to create cluster.*rolled back`),
			},
		},
		CheckDestroy: func(*terraform.State) error {
			if out := testAccDocker(t, "ps", "-a", "--filter", "label=k3d.cluster="+name, "--format", "{{.Names}}"); out != "" {
				return fmt.Errorf("containers left behind:\n%s", out)
			}
			return nil
		},
	})
}

func TestAccCluster_validation(t *testing.T) {
	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{name: "moving image tag", extra: `image = "rancher/k3s:latest"`, wantErr: `moving tag`},
		{name: "duplicate container port", extra: `
  image = "` + testImage + `"
  ports = [
    { host_port = 8080, container_port = 80 },
    { host_port = 8081, container_port = 80 },
  ]`, wantErr: `mapped more than once`},
		{name: "host network", extra: `
  image   = "` + testImage + `"
  network = "host"`, wantErr: `value must be none of`},
		{name: "bad timeout", extra: `
  image   = "` + testImage + `"
  timeout = "soon"`, wantErr: `invalid duration`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccFactories,
				TerraformVersionChecks:   testAccVersionChecks,
				Steps: []resource.TestStep{
					{
						Config: fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name = "tfacc-invalid"
  %s
}
`, tt.extra),
						ExpectError: regexp.MustCompile(tt.wantErr),
						PlanOnly:    true,
					},
				},
			})
		})
	}
}

func TestAccRegistry_withCluster(t *testing.T) {
	registryName := testAccName(t)
	clusterName := testAccName(t)
	port := testAccFreePort(t)
	registryAddress := "k3d_registry.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy: func(*terraform.State) error {
			if _, err := k3dclient.GetRegistry(context.Background(), registryName); err == nil {
				return fmt.Errorf("registry %s still exists", registryName)
			}
			return testAccClusterGone(clusterName)(nil)
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "k3d_registry" "test" {
  name           = %q
  port           = %d
  delete_enabled = true
}

resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false

  registries = {
    use = [k3d_registry.test.ref]
  }
}

data "k3d_registry" "test" {
  name = k3d_registry.test.name

  depends_on = [k3d_cluster.test]
}
`, registryName, port, clusterName, testImage),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(registryAddress, "node_name", "k3d-"+registryName),
					resource.TestCheckResourceAttr(registryAddress, "ref", "k3d-"+registryName+":5000"),
					resource.TestCheckResourceAttr(registryAddress, "port", fmt.Sprint(port)),
					resource.TestCheckResourceAttr(registryAddress, "image", k3dclient.DefaultRegistryImage),
					resource.TestCheckResourceAttrPair("data.k3d_registry.test", "ref", registryAddress, "ref"),
					resource.TestCheckTypeSetElemAttr("data.k3d_registry.test", "networks.*", "k3d-"+clusterName),
					testAccCheck(func() error {
						env := testAccDocker(t, "inspect", "-f", "{{.Config.Env}}", "k3d-"+registryName)
						if !strings.Contains(env, "REGISTRY_STORAGE_DELETE_ENABLED=true") {
							return fmt.Errorf("delete_enabled not applied")
						}
						config := testAccDocker(t, "exec", "k3d-"+clusterName+"-server-0", "cat", "/etc/rancher/k3s/registries.yaml")
						if !strings.Contains(config, "k3d-"+registryName) {
							return fmt.Errorf("registries.yaml does not mention the registry:\n%s", config)
						}
						return nil
					}),
				),
			},
			{
				ResourceName:            registryAddress,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"delete_enabled", "network"},
			},
		},
	})
}

func TestAccRegistry_proxyImportAdopts(t *testing.T) {
	name := testAccName(t)
	address := "k3d_registry.test"
	port := testAccFreePort(t)
	config := fmt.Sprintf(`
resource "k3d_registry" "test" {
  name = %q
  port = %d

  proxy = {
    remote_url = "https://registry-1.docker.io"
  }
}
`, name, port)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: testAccCheck(func() error {
					env := testAccDocker(t, "inspect", "-f", "{{.Config.Env}}", "k3d-"+name)
					if !strings.Contains(env, "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io") {
						return fmt.Errorf("proxy not applied: %s", env)
					}
					return nil
				}),
			},
			{
				ResourceName:            address,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"delete_enabled", "network", "proxy", "proxy.%", "proxy.remote_url"},
			},
		},
	})
}
