// Package helm wraps the Helm SDK behind a small interface shaped for the
// Terraform provider, using a kubeconfig held in memory so that credentials are
// never written to disk.
package helm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/kube"
	release "helm.sh/helm/v4/pkg/release"
	releasev1 "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
)

const storageDriver = "secret"

// ErrNotFound reports that the API answered and the release does not exist.
var ErrNotFound = errors.New("release not found")

// Release is what can be read back from an installed release.
type Release struct {
	Name         string
	Namespace    string
	Revision     int
	Status       string
	ChartName    string
	ChartVersion string
	AppVersion   string
}

// Options controls an install or an upgrade.
type Options struct {
	Name            string
	Namespace       string
	Wait            bool
	WaitForJobs     bool
	Atomic          bool
	CleanupOnFail   bool
	CreateNamespace bool
	TakeOwnership   bool
	MaxHistory      int
	Timeout         time.Duration
}

// Client manages releases in one namespace of one cluster.
type Client struct {
	cfg *action.Configuration
}

// New returns a client for a namespace using a kubeconfig's contents.
func New(kubeconfig, namespace string) (*Client, error) {
	getter, err := newRESTClientGetter(kubeconfig, namespace)
	if err != nil {
		return nil, err
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(handler))
	if err := cfg.Init(getter, namespace, storageDriver); err != nil {
		return nil, fmt.Errorf("initialize helm: %w", err)
	}
	return &Client{cfg: cfg}, nil
}

func waitStrategy(wait bool) kube.WaitStrategy {
	if wait {
		return kube.StatusWatcherStrategy
	}
	return kube.HookOnlyStrategy
}

// Get reads the latest revision of a release. It returns ErrNotFound only when
// the API answered, so an unreachable API is always a different error.
func (c *Client) Get(name string) (*Release, error) {
	rel, err := action.NewGet(c.cfg).Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get release %s: %w", name, err)
	}
	return toRelease(rel)
}

// Install creates a release. On failure it still returns the release if one was
// recorded, so that the caller can keep track of it.
func (c *Client) Install(ctx context.Context, ch chart.Charter, values map[string]any, opts Options) (*Release, error) {
	install := action.NewInstall(c.cfg)
	install.ReleaseName = opts.Name
	install.Namespace = opts.Namespace
	install.WaitStrategy = waitStrategy(opts.Wait)
	install.WaitForJobs = opts.WaitForJobs
	install.RollbackOnFailure = opts.Atomic
	install.CreateNamespace = opts.CreateNamespace
	install.TakeOwnership = opts.TakeOwnership
	install.Timeout = opts.Timeout

	rel, err := install.RunWithContext(ctx, ch, values)
	if err != nil {
		return c.afterFailure(opts.Name), fmt.Errorf("install release %s: %w", opts.Name, err)
	}
	return toRelease(rel)
}

// Upgrade moves a release to a chart and values. The values replace the previous
// ones entirely, so the release always matches the configuration.
func (c *Client) Upgrade(ctx context.Context, ch chart.Charter, values map[string]any, opts Options) (*Release, error) {
	upgrade := action.NewUpgrade(c.cfg)
	upgrade.Namespace = opts.Namespace
	upgrade.WaitStrategy = waitStrategy(opts.Wait)
	upgrade.WaitForJobs = opts.WaitForJobs
	upgrade.RollbackOnFailure = opts.Atomic
	upgrade.CleanupOnFail = opts.CleanupOnFail
	upgrade.TakeOwnership = opts.TakeOwnership
	upgrade.MaxHistory = opts.MaxHistory
	upgrade.Timeout = opts.Timeout
	upgrade.ResetValues = true

	rel, err := upgrade.RunWithContext(ctx, opts.Name, ch, values)
	if err != nil {
		return c.afterFailure(opts.Name), fmt.Errorf("upgrade release %s: %w", opts.Name, err)
	}
	return toRelease(rel)
}

func (c *Client) afterFailure(name string) *Release {
	rel, err := c.Get(name)
	if err != nil {
		return nil
	}
	return rel
}

// Uninstall removes a release. Removing a missing release is not an error.
func (c *Client) Uninstall(name string, wait bool, timeout time.Duration) error {
	uninstall := action.NewUninstall(c.cfg)
	uninstall.IgnoreNotFound = true
	uninstall.WaitStrategy = waitStrategy(wait)
	uninstall.Timeout = timeout

	if _, err := uninstall.Run(name); err != nil {
		return fmt.Errorf("uninstall release %s: %w", name, err)
	}
	return nil
}

func toRelease(rel release.Releaser) (*Release, error) {
	concrete, ok := rel.(*releasev1.Release)
	if !ok || concrete == nil {
		return nil, fmt.Errorf("unexpected release type %T", rel)
	}

	out := &Release{
		Name:      concrete.Name,
		Namespace: concrete.Namespace,
		Revision:  concrete.Version,
	}
	if concrete.Info != nil {
		out.Status = string(concrete.Info.Status)
	}
	if concrete.Chart != nil && concrete.Chart.Metadata != nil {
		out.ChartName = concrete.Chart.Metadata.Name
		out.ChartVersion = concrete.Chart.Metadata.Version
		out.AppVersion = concrete.Chart.Metadata.AppVersion
	}
	return out, nil
}
