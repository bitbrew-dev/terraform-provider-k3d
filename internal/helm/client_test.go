package helm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

const testKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
users:
- name: u
  user: {}
`

func fakeClient(t *testing.T) *Client {
	t.Helper()
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(slog.NewTextHandler(io.Discard, nil)))
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.KubeClient = &kubefake.PrintingKubeClient{Out: io.Discard}
	cfg.Capabilities = common.DefaultCapabilities
	return &Client{cfg: cfg}
}

func testChart(version string) *chartv2.Chart {
	return &chartv2.Chart{
		Metadata: &chartv2.Metadata{
			APIVersion: chartv2.APIVersionV2,
			Name:       "demo",
			Version:    version,
			AppVersion: "1.2.3",
		},
	}
}

func testOptions() Options {
	return Options{Name: "demo", Namespace: "default", MaxHistory: 10, Timeout: time.Minute}
}

func TestGetMissingIsNotFound(t *testing.T) {
	_, err := fakeClient(t).Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInstallUpgradeUninstall(t *testing.T) {
	ctx := context.Background()
	client := fakeClient(t)

	installed, err := client.Install(ctx, testChart("1.0.0"), map[string]any{"a": 1}, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := Release{
		Name:         "demo",
		Namespace:    "default",
		Revision:     1,
		Status:       "deployed",
		ChartName:    "demo",
		ChartVersion: "1.0.0",
		AppVersion:   "1.2.3",
	}
	if *installed != want {
		t.Errorf("installed = %+v, want %+v", *installed, want)
	}

	upgraded, err := client.Upgrade(ctx, testChart("1.1.0"), nil, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Revision != 2 || upgraded.ChartVersion != "1.1.0" {
		t.Errorf("upgraded = %+v", *upgraded)
	}

	read, err := client.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	if *read != *upgraded {
		t.Errorf("read = %+v, want %+v", *read, *upgraded)
	}

	if err := client.Uninstall("demo", false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("demo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after uninstall err = %v, want ErrNotFound", err)
	}
}

func TestUpgradeReplacesPreviousValues(t *testing.T) {
	ctx := context.Background()
	client := fakeClient(t)

	if _, err := client.Install(ctx, testChart("1.0.0"), map[string]any{"old": "x"}, testOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upgrade(ctx, testChart("1.0.0"), map[string]any{"new": "y"}, testOptions()); err != nil {
		t.Fatal(err)
	}

	rel, err := action.NewGetValues(client.cfg).Run("demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := rel["old"]; stale {
		t.Errorf("values kept a removed key: %v", rel)
	}
	if rel["new"] != "y" {
		t.Errorf("values = %v", rel)
	}
}

func TestFailedInstallStillReportsTheRelease(t *testing.T) {
	client := fakeClient(t)
	client.cfg.KubeClient = &kubefake.FailingKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard},
		WaitError:          errors.New("not ready"),
	}

	opts := testOptions()
	opts.Wait = true
	rel, err := client.Install(context.Background(), testChart("1.0.0"), nil, opts)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v", err)
	}
	if rel == nil || rel.Status != "failed" {
		t.Errorf("rel = %+v, want a failed release", rel)
	}
}

func TestUninstallMissingIsNotAnError(t *testing.T) {
	if err := fakeClient(t).Uninstall("missing", false, time.Minute); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestWaitStrategy(t *testing.T) {
	if got := waitStrategy(true); got != kube.StatusWatcherStrategy {
		t.Errorf("wait = %v", got)
	}
	if got := waitStrategy(false); got != kube.HookOnlyStrategy {
		t.Errorf("no wait = %v", got)
	}
}

func TestNewRESTClientGetter(t *testing.T) {
	getter, err := newRESTClientGetter(testKubeconfig, "apps")
	if err != nil {
		t.Fatal(err)
	}

	namespace, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		t.Fatal(err)
	}
	if namespace != "apps" {
		t.Errorf("namespace = %q, want apps", namespace)
	}

	config, err := getter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "https://127.0.0.1:1" {
		t.Errorf("host = %q", config.Host)
	}
}

func TestNewRejectsBadKubeconfig(t *testing.T) {
	tests := map[string]string{
		"invalid yaml":       "not: [a kubeconfig",
		"no current context": "apiVersion: v1\nkind: Config\n",
	}
	for name, kubeconfig := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(kubeconfig, "default"); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
