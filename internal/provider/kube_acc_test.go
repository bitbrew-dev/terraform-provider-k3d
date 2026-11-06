package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testJobImage = "busybox:1.36"

var (
	sharedClusterOnce sync.Once
	sharedClusterName string
	errSharedCluster  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedClusterName != "" {
		if err := k3dclient.DeleteCluster(context.Background(), sharedClusterName); err != nil {
			fmt.Fprintf(os.Stderr, "delete shared cluster: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// testAccSharedCluster returns a cluster for object tests. Tests that prove an
// object can share an apply with its cluster create their own instead.
func testAccSharedCluster(t *testing.T) string {
	t.Helper()
	testAccRequire(t)
	sharedClusterOnce.Do(func() {
		name := "tfacc-shared-" + acctest.RandStringFromCharSet(6, acctest.CharSetAlphaNum)
		_, errSharedCluster = k3dclient.CreateCluster(context.Background(), k3dclient.ClusterSpec{
			Name:    name,
			Image:   testImage,
			Servers: 1,
			Timeout: 10 * time.Minute,
		})
		if errSharedCluster == nil {
			sharedClusterName = name
		}
	})
	if errSharedCluster != nil {
		t.Fatal(errSharedCluster)
	}
	return sharedClusterName
}

func testAccKubeconfigFile(t *testing.T, cluster string) string {
	t.Helper()
	raw, err := k3dclient.GetKubeconfig(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAccKubectlRaw(t *testing.T, cluster string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--kubeconfig", testAccKubeconfigFile(t, cluster), "--request-timeout=30s"}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func testAccKubectl(t *testing.T, cluster string, args ...string) string {
	t.Helper()
	out, err := testAccKubectlRaw(t, cluster, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func testAccExpectKubectl(t *testing.T, cluster, want string, args ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		if got := testAccKubectl(t, cluster, args...); got != want {
			return fmt.Errorf("kubectl %s = %q, want %q", strings.Join(args, " "), got, want)
		}
		return nil
	}
}

func testAccObjectName() string {
	return "tfacc-" + strings.ToLower(acctest.RandStringFromCharSet(8, acctest.CharSetAlphaNum))
}

func testAccCaseFor(steps ...resource.TestStep) resource.TestCase {
	return resource.TestCase{
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		Steps:                    steps,
	}
}

func expectAction(address string, action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, action)},
	}
}

// TestAccKube_oneApplyAndClusterReplace proves everything can be created in one
// apply from nothing, and that replacing a cluster recreates its objects instead
// of leaving Terraform believing they still exist.
func TestAccKube_oneApplyAndClusterReplace(t *testing.T) {
	name := testAccName(t)

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
  labels = {
    purpose = "applications"
  }
}

resource "k3d_config_map" "settings" {
  cluster   = k3d_cluster.test.id
  namespace = k3d_namespace.app.name
  name      = "settings"
  data = {
    mode = "test"
  }
}

resource "k3d_secret" "credentials" {
  cluster   = k3d_cluster.test.id
  namespace = k3d_namespace.app.name
  name      = "credentials"
  data = {
    token = "abc"
  }
}

resource "k3d_job" "migrate" {
  cluster   = k3d_cluster.test.id
  namespace = k3d_namespace.app.name
  name      = "migrate"
  image     = %q
  command   = ["sh", "-c", "echo done"]
}

resource "k3d_manifest" "account" {
  cluster  = k3d_cluster.test.id
  manifest = <<-YAML
    apiVersion: v1
    kind: ServiceAccount
    metadata:
      name: app
      namespace: ${k3d_namespace.app.name}
  YAML
}
`, name, testImage, agents, testJobImage)
	}

	objectsExist := func() error {
		for _, object := range []string{"configmap/settings", "secret/credentials", "job/migrate", "serviceaccount/app"} {
			testAccKubectl(t, name, "get", "-n", "app", object, "-o", "name")
		}
		if got := testAccKubectl(t, name, "get", "namespace", "app", "-o", "jsonpath={.metadata.labels.purpose}"); got != "applications" {
			return fmt.Errorf("namespace label = %q", got)
		}
		return nil
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{
				Config: config(0),
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheck(objectsExist),
					resource.TestCheckResourceAttr("k3d_namespace.app", "id", name+"/app"),
					resource.TestCheckResourceAttr("k3d_manifest.account", "kind", "ServiceAccount"),
					resource.TestCheckResourceAttr("k3d_manifest.account", "namespace", "app"),
				),
			},
			{
				Config: config(1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("k3d_cluster.test", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_namespace.app", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_config_map.settings", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_secret.credentials", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_job.migrate", plancheck.ResourceActionReplace),
						plancheck.ExpectResourceAction("k3d_manifest.account", plancheck.ResourceActionReplace),
					},
				},
				Check: testAccCheck(objectsExist),
			},
		},
	})
}

// TestAccManifest_crdAndCustomResource proves a custom resource can share an
// apply with both its CRD and the cluster, the case kubernetes_manifest cannot do.
func TestAccManifest_crdAndCustomResource(t *testing.T) {
	name := testAccName(t)

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
  traefik = false
}

resource "k3d_manifest" "crd" {
  cluster  = k3d_cluster.test.id
  manifest = <<-YAML
    apiVersion: apiextensions.k8s.io/v1
    kind: CustomResourceDefinition
    metadata:
      name: widgets.tfacc.example.com
    spec:
      group: tfacc.example.com
      scope: Namespaced
      names:
        plural: widgets
        singular: widget
        kind: Widget
      versions:
        - name: v1
          served: true
          storage: true
          schema:
            openAPIV3Schema:
              type: object
              properties:
                spec:
                  type: object
                  x-kubernetes-preserve-unknown-fields: true
  YAML
}

resource "k3d_manifest" "widget" {
  cluster  = k3d_cluster.test.id
  manifest = <<-YAML
    apiVersion: tfacc.example.com/v1
    kind: Widget
    metadata:
      name: first
    spec:
      size: 3
  YAML

  depends_on = [k3d_manifest.crd]
}
`, name, testImage),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("k3d_manifest.crd", "namespace"),
					resource.TestCheckResourceAttr("k3d_manifest.widget", "namespace", "default"),
					testAccExpectKubectl(t, name, "3", "get", "widget", "first", "-n", "default", "-o", "jsonpath={.spec.size}"),
				),
			},
		},
	})
}

func TestAccKube_clusterDeletedOutsideTerraform(t *testing.T) {
	name := testAccName(t)
	config := fmt.Sprintf(`
resource "k3d_cluster" "test" {
  name    = %q
  image   = %q
  traefik = false
}

resource "k3d_namespace" "app" {
  cluster = k3d_cluster.test.id
  name    = "app"
}
`, name, testImage)

	deleteCluster := func() {
		if err := k3dclient.DeleteCluster(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccFactories,
		TerraformVersionChecks:   testAccVersionChecks,
		CheckDestroy:             testAccClusterGone(name),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: deleteCluster,
				Config:    config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("k3d_cluster.test", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("k3d_namespace.app", plancheck.ResourceActionCreate),
					},
				},
			},
			{
				PreConfig: deleteCluster,
				Config:    config,
				Destroy:   true,
			},
		},
	})
}

func TestAccNamespace_lifecycle(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_namespace.test"

	config := func(labels string) string {
		return fmt.Sprintf(`
resource "k3d_namespace" "test" {
  cluster = %q
  name    = %q
  labels  = { %s }
}
`, cluster, name, labels)
	}
	label := func(key string) []string {
		return []string{"get", "namespace", name, "-o", fmt.Sprintf("jsonpath={.metadata.labels.%s}", key)}
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(`team = "a"`),
			Check:  testAccExpectKubectl(t, cluster, "a", label("team")...),
		},
		resource.TestStep{
			PreConfig: func() { testAccKubectl(t, cluster, "label", "namespace", name, "added-by=controller") },
			Config:    config(`team = "a"`),
			PlanOnly:  true,
		},
		resource.TestStep{
			PreConfig:        func() { testAccKubectl(t, cluster, "label", "namespace", name, "team-") },
			Config:           config(`team = "a"`),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccExpectKubectl(t, cluster, "a", label("team")...),
				testAccExpectKubectl(t, cluster, "controller", label("added-by")...),
			),
		},
		resource.TestStep{
			Config:           config(`owner = "b"`),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccExpectKubectl(t, cluster, "", label("team")...),
				testAccExpectKubectl(t, cluster, "b", label("owner")...),
				testAccExpectKubectl(t, cluster, "controller", label("added-by")...),
			),
		},
		resource.TestStep{
			ResourceName:            address,
			ImportState:             true,
			ImportStateId:           cluster + "/" + name,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"labels", "labels.%", "labels.owner"},
		},
	))

	if out, err := testAccKubectlRaw(t, cluster, "get", "namespace", name); err == nil {
		t.Errorf("namespace still exists after destroy: %s", out)
	}
}

func TestAccNamespace_existingIsRejected(t *testing.T) {
	cluster := testAccSharedCluster(t)

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
resource "k3d_namespace" "test" {
  cluster = %q
  name    = "default"
}
`, cluster),
		ExpectError: regexp.MustCompile(`already exists: import it`),
	}))
}

func TestAccConfigMap_lifecycle(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_config_map.test"

	config := func(data string) string {
		return fmt.Sprintf(`
resource "k3d_config_map" "test" {
  cluster = %q
  name    = %q
  data    = { %s }
}
`, cluster, name, data)
	}
	value := func(key string) []string {
		return []string{"get", "configmap", name, "-n", "default", "-o", fmt.Sprintf("jsonpath={.data.%s}", key)}
	}

	var uid string
	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(`a = "1", b = "2"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(address, "namespace", "default"),
				testAccExpectKubectl(t, cluster, "1", value("a")...),
				testAccCheck(func() error {
					uid = testAccKubectl(t, cluster, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}")
					return nil
				}),
			),
		},
		resource.TestStep{
			Config:           config(`a = "changed"`),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccExpectKubectl(t, cluster, "changed", value("a")...),
				testAccExpectKubectl(t, cluster, "", value("b")...),
				testAccCheck(func() error {
					got := testAccKubectl(t, cluster, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}")
					if got != uid {
						return fmt.Errorf("config map was recreated")
					}
					return nil
				}),
			),
		},
		resource.TestStep{
			PreConfig: func() {
				testAccKubectl(t, cluster, "patch", "configmap", name, "-n", "default", "--type=merge", "-p", `{"data":{"a":"edited"}}`)
			},
			Config:           config(`a = "changed"`),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check:            testAccExpectKubectl(t, cluster, "changed", value("a")...),
		},
		resource.TestStep{
			ResourceName:            address,
			ImportState:             true,
			ImportStateId:           cluster + "/default/" + name,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"data", "data.%", "data.a"},
		},
	))
}

func TestAccConfigMap_kubeconfig(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	kubeconfig := testAccKubeconfigFile(t, cluster)

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
resource "k3d_config_map" "test" {
  kubeconfig = file(%q)
  name       = %q
  data       = { a = "1" }
}
`, kubeconfig, name),
		Check: testAccExpectKubectl(t, cluster, "1", "get", "configmap", name, "-n", "default", "-o", "jsonpath={.data.a}"),
	}))
}

func TestAccSecret_data(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_secret.test"

	config := func(value string) string {
		return fmt.Sprintf(`
resource "k3d_secret" "test" {
  cluster = %q
  name    = %q
  labels  = { "argocd.argoproj.io/secret-type" = "repository" }
  data    = { token = %q }
}
`, cluster, name, value)
	}
	secretValue := func() (string, error) {
		encoded := testAccKubectl(t, cluster, "get", "secret", name, "-n", "default", "-o", "jsonpath={.data.token}")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		return string(decoded), err
	}
	expectValue := func(want string) resource.TestCheckFunc {
		return testAccCheck(func() error {
			got, err := secretValue()
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("token = %q, want %q", got, want)
			}
			return nil
		})
	}

	var firstHash string
	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config("one"),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectValue("one"),
				resource.TestCheckResourceAttr(address, "type", "Opaque"),
				func(s *terraform.State) error {
					firstHash = s.RootModule().Resources[address].Primary.Attributes["data_hash"]
					if firstHash == "" {
						return fmt.Errorf("data_hash is empty")
					}
					return nil
				},
			),
		},
		resource.TestStep{
			Config:           config("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectValue("two"),
				func(s *terraform.State) error {
					if s.RootModule().Resources[address].Primary.Attributes["data_hash"] == firstHash {
						return fmt.Errorf("data_hash did not change")
					}
					return nil
				},
			),
		},
		resource.TestStep{
			PreConfig: func() {
				edited := base64.StdEncoding.EncodeToString([]byte("edited"))
				testAccKubectl(t, cluster, "patch", "secret", name, "-n", "default", "--type=merge", "-p", fmt.Sprintf(`{"data":{"token":%q}}`, edited))
			},
			Config:           config("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check:            expectValue("two"),
		},
		resource.TestStep{
			ResourceName:            address,
			ImportState:             true,
			ImportStateId:           cluster + "/default/" + name,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"data", "data.%", "data.token", "data_hash", "labels", "labels.%", "labels.argocd.argoproj.io/secret-type"},
		},
	))
}

func TestAccSecret_pullSecret(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()

	resource.Test(t, testAccCaseFor(resource.TestStep{
		Config: fmt.Sprintf(`
resource "k3d_secret" "test" {
  cluster = %q
  name    = %q
  type    = "kubernetes.io/dockerconfigjson"
  data = {
    ".dockerconfigjson" = jsonencode({
      auths = {
        "ghcr.io" = { auth = base64encode("user:pass") }
      }
    })
  }
}
`, cluster, name),
		Check: testAccExpectKubectl(t, cluster, "kubernetes.io/dockerconfigjson", "get", "secret", name, "-n", "default", "-o", "jsonpath={.type}"),
	}))
}

// TestAccSecret_writeOnly proves write-only values reach the cluster but never
// Terraform state, and that only a version bump applies a new value.
func TestAccSecret_writeOnly(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_secret.test"
	const (
		firstValue  = "first-plaintext-value"
		secondValue = "second-plaintext-value"
	)

	config := func(value string, version int) string {
		return fmt.Sprintf(`
resource "k3d_secret" "test" {
  cluster         = %q
  name            = %q
  data_wo         = { token = %q }
  data_wo_version = %d
}
`, cluster, name, value, version)
	}
	expectValue := func(want string) resource.TestCheckFunc {
		return testAccCheck(func() error {
			encoded := testAccKubectl(t, cluster, "get", "secret", name, "-n", "default", "-o", "jsonpath={.data.token}")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return err
			}
			if string(decoded) != want {
				return fmt.Errorf("token = %q, want %q", decoded, want)
			}
			return nil
		})
	}
	expectNotInState := func(value string) resource.TestCheckFunc {
		return func(s *terraform.State) error {
			attributes := s.RootModule().Resources[address].Primary.Attributes
			for key, got := range attributes {
				if strings.Contains(got, value) {
					return fmt.Errorf("plaintext found in state attribute %s", key)
				}
			}
			if hash := attributes["data_hash"]; hash != hashString("version:"+attributes["data_wo_version"]) {
				return fmt.Errorf("data_hash %s is not derived from the version alone", hash)
			}
			return nil
		}
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(firstValue, 1),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectValue(firstValue),
				expectNotInState(firstValue),
				resource.TestCheckNoResourceAttr(address, "data_wo.%"),
			),
		},
		resource.TestStep{
			Config:   config(secondValue, 1),
			PlanOnly: true,
		},
		resource.TestStep{
			Config:           config(secondValue, 2),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				expectValue(secondValue),
				expectNotInState(secondValue),
			),
		},
	))
}

func TestAccJob_success(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_job.test"

	config := func(message string) string {
		return fmt.Sprintf(`
resource "k3d_job" "test" {
  cluster = %q
  name    = %q
  image   = %q
  command = ["sh", "-c", "echo $MESSAGE"]
  env     = { MESSAGE = %q }
}
`, cluster, name, testJobImage, message)
	}

	logs := func(want string) resource.TestCheckFunc {
		return testAccCheck(func() error {
			got := testAccKubectl(t, cluster, "logs", "job/"+name, "-n", "default")
			if got != want {
				return fmt.Errorf("logs = %q, want %q", got, want)
			}
			return nil
		})
	}
	expectComplete := testAccExpectKubectl(t, cluster, "True", "get", "job", name, "-n", "default", "-o", `jsonpath={.status.conditions[?(@.type=="Complete")].status}`)

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config("one"),
			Check:  resource.ComposeAggregateTestCheckFunc(expectComplete, logs("one")),
		},
		resource.TestStep{
			Config:           config("two"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionReplace),
			Check:            resource.ComposeAggregateTestCheckFunc(expectComplete, logs("two")),
		},
	))
}

// TestAccJob_importAdoptsConfig proves importing a finished job and writing its
// config does not re-run it.
func TestAccJob_importAdoptsConfig(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_job.test"
	config := fmt.Sprintf(`
resource "k3d_job" "test" {
  cluster = %q
  name    = %q
  image   = %q
  command = ["true"]
}
`, cluster, name, testJobImage)

	testAccKubectl(t, cluster, "create", "job", name, "-n", "default", "--image="+testJobImage, "--", "true")
	uid := testAccKubectl(t, cluster, "get", "job", name, "-n", "default", "-o", "jsonpath={.metadata.uid}")

	resource.Test(t, testAccCaseFor(
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
			Check:            testAccExpectKubectl(t, cluster, uid, "get", "job", name, "-n", "default", "-o", "jsonpath={.metadata.uid}"),
		},
	))
}

func TestAccJob_failureTaints(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()

	config := func(script string) string {
		return fmt.Sprintf(`
resource "k3d_job" "test" {
  cluster       = %q
  name          = %q
  image         = %q
  command       = ["sh", "-c", %q]
  backoff_limit = 0
}
`, cluster, name, testJobImage, script)
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config:      config("exit 3"),
			ExpectError: regexp.MustCompile(`(?s)Job did not complete.*exit code 3`),
		},
		resource.TestStep{
			Config:           config("exit 3"),
			ConfigPlanChecks: expectAction("k3d_job.test", plancheck.ResourceActionReplace),
			ExpectError:      regexp.MustCompile(`Job did not complete`),
		},
		resource.TestStep{
			Config:           config("exit 0"),
			ConfigPlanChecks: expectAction("k3d_job.test", plancheck.ResourceActionReplace),
		},
	))
}

func TestAccJob_ttlDoesNotRerun(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	config := fmt.Sprintf(`
resource "k3d_job" "test" {
  cluster                    = %q
  name                       = %q
  image                      = %q
  command                    = ["true"]
  ttl_seconds_after_finished = 10
}
`, cluster, name, testJobImage)

	resource.Test(t, testAccCaseFor(
		resource.TestStep{Config: config},
		resource.TestStep{
			PreConfig: func() {
				for range 60 {
					if _, err := testAccKubectlRaw(t, cluster, "get", "job", name, "-n", "default"); err != nil {
						return
					}
					time.Sleep(time.Second)
				}
				t.Fatal("job was not removed by its TTL")
			},
			Config:   config,
			PlanOnly: true,
		},
	))
}

func TestAccManifest_lifecycle(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_manifest.test"

	config := func(deployment string, replicas int) string {
		return fmt.Sprintf(`
resource "k3d_manifest" "test" {
  cluster = %q
  wait    = true
  timeout = "4m"
  manifest = yamlencode({
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = %q, namespace = "default" }
    spec = {
      replicas = %d
      selector = { matchLabels = { app = %q } }
      template = {
        metadata = { labels = { app = %q } }
        spec = {
          containers = [{ name = "main", image = %q, command = ["sleep", "3600"] }]
        }
      }
    }
  })
}
`, cluster, deployment, replicas, name, name, testJobImage)
	}

	var uid string
	readUID := func(s *terraform.State) error {
		uid = s.RootModule().Resources[address].Primary.ID
		if uid == "" {
			return fmt.Errorf("id is empty")
		}
		return nil
	}

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config: config(name, 1),
			Check: resource.ComposeAggregateTestCheckFunc(
				readUID,
				resource.TestCheckResourceAttr(address, "api_version", "apps/v1"),
				resource.TestCheckResourceAttr(address, "kind", "Deployment"),
				resource.TestCheckResourceAttr(address, "namespace", "default"),
				resource.TestCheckResourceAttr(address, "name", name),
				testAccExpectKubectl(t, cluster, "1", "get", "deployment", name, "-n", "default", "-o", "jsonpath={.status.readyReplicas}"),
			),
		},
		resource.TestStep{
			Config:           config(name, 2),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccExpectKubectl(t, cluster, "2", "get", "deployment", name, "-n", "default", "-o", "jsonpath={.status.readyReplicas}"),
				func(s *terraform.State) error {
					if got := s.RootModule().Resources[address].Primary.ID; got != uid {
						return fmt.Errorf("object was recreated: %s, was %s", got, uid)
					}
					return nil
				},
			),
		},
		resource.TestStep{
			Config:           config(name+"-renamed", 1),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionReplace),
			Check: testAccCheck(func() error {
				if _, err := testAccKubectlRaw(t, cluster, "get", "deployment", name, "-n", "default"); err == nil {
					return fmt.Errorf("old deployment still exists")
				}
				return nil
			}),
		},
		resource.TestStep{
			PreConfig: func() {
				testAccKubectl(t, cluster, "delete", "deployment", name+"-renamed", "-n", "default", "--wait=true")
			},
			Config:           config(name+"-renamed", 1),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionCreate),
		},
	))
}

func TestAccManifest_importAndExisting(t *testing.T) {
	cluster := testAccSharedCluster(t)
	name := testAccObjectName()
	address := "k3d_manifest.test"
	configMap := func(value string) string {
		return fmt.Sprintf(`
resource "k3d_manifest" "test" {
  cluster  = %q
  manifest = yamlencode({
    apiVersion = "v1"
    kind       = "ConfigMap"
    metadata   = { name = %q, namespace = "default" }
    data       = { key = %q }
  })
}
`, cluster, name, value)
	}

	testAccKubectl(t, cluster, "create", "configmap", name, "-n", "default", "--from-literal=key=outside")
	originalUID := testAccKubectl(t, cluster, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}")

	resource.Test(t, testAccCaseFor(
		resource.TestStep{
			Config:      configMap("managed"),
			ExpectError: regexp.MustCompile(`already exists: import it`),
		},
		resource.TestStep{
			Config:             configMap("managed"),
			ResourceName:       address,
			ImportState:        true,
			ImportStateId:      strings.Join([]string{cluster, "v1", "ConfigMap", "default", name}, "|"),
			ImportStatePersist: true,
		},
		resource.TestStep{
			Config:           configMap("managed"),
			ConfigPlanChecks: expectAction(address, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				testAccExpectKubectl(t, cluster, "managed", "get", "configmap", name, "-n", "default", "-o", "jsonpath={.data.key}"),
				testAccExpectKubectl(t, cluster, originalUID, "get", "configmap", name, "-n", "default", "-o", "jsonpath={.metadata.uid}"),
			),
		},
	))
}

func TestAccKube_validation(t *testing.T) {
	const connection = "\n  cluster = \"tfacc-unused\""

	tests := []struct {
		name     string
		resource string
		body     string
		wantErr  string
	}{
		{
			name:     "no connection",
			resource: "k3d_namespace",
			body:     `name = "a"`,
			wantErr:  `Exactly one of these attributes must be configured: \[cluster,kubeconfig\]`,
		},
		{
			name:     "both connections",
			resource: "k3d_namespace",
			body:     "name = \"a\"\n  kubeconfig = \"k\"" + connection,
			wantErr:  `Exactly one of these attributes must be configured: \[cluster,kubeconfig\]`,
		},
		{
			name:     "secret with no data",
			resource: "k3d_secret",
			body:     `name = "a"` + connection,
			wantErr:  `Exactly one of these attributes must be configured: \[data,data_wo\]`,
		},
		{
			name:     "secret with both data modes",
			resource: "k3d_secret",
			body:     "name = \"a\"\n  data = {}\n  data_wo = {}\n  data_wo_version = 1" + connection,
			wantErr:  `Exactly one of these attributes must be configured: \[data,data_wo\]`,
		},
		{
			name:     "write-only without a version",
			resource: "k3d_secret",
			body:     "name = \"a\"\n  data_wo = {}" + connection,
			wantErr:  `data_wo_version`,
		},
		{
			name:     "job with a moving image",
			resource: "k3d_job",
			body:     "name = \"a\"\n  image = \"busybox:latest\"" + connection,
			wantErr:  `moving tag`,
		},
		{
			name:     "manifest with two objects",
			resource: "k3d_manifest",
			body:     `manifest = "kind: A\n---\nkind: B\n"` + connection,
			wantErr:  `more than one object`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource.Test(t, testAccCaseFor(resource.TestStep{
				Config:      fmt.Sprintf("resource %q \"t\" {\n  %s\n}\n", tt.resource, tt.body),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(tt.wantErr),
			}))
		})
	}
}
