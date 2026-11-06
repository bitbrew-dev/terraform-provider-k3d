package k3dclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	cliutil "github.com/k3d-io/k3d/v5/cmd/util"
	"github.com/k3d-io/k3d/v5/pkg/client"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

const registryNamePrefix = k3d.DefaultObjectNamePrefix + "-"

// DefaultRegistryImage is the image k3d uses for registries.
const DefaultRegistryImage = k3d.DefaultRegistryImageRepo + ":" + k3d.DefaultRegistryImageTag

// DefaultRegistryNetwork is the network a registry joins when none is set.
const DefaultRegistryNetwork = k3d.DefaultRuntimeNetwork

// RegistrySpec is the desired configuration of a registry.
type RegistrySpec struct {
	Name           string
	Image          string
	HostIP         string
	Port           int
	Network        string
	Volumes        []string
	DeleteEnabled  bool
	ProxyRemoteURL string
	ProxyUsername  string
	ProxyPassword  string
}

// RegistryState is what can be read back from a registry.
type RegistryState struct {
	Name     string
	NodeName string
	Ref      string
	Image    string
	HostIP   string
	Port     int
	Networks []string
}

// CreateRegistry creates a registry and returns its state.
func CreateRegistry(ctx context.Context, spec RegistrySpec) (*RegistryState, error) {
	if _, err := findRegistryNode(ctx, spec.Name); err == nil {
		return nil, fmt.Errorf("registry %q already exists: import it or choose another name", spec.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	exposure, err := cliutil.ParsePortExposureSpec(fmt.Sprintf("%s:%d", spec.HostIP, spec.Port), k3d.DefaultRegistryPort, false)
	if err != nil {
		return nil, fmt.Errorf("parse registry port: %w", err)
	}

	registry := &k3d.Registry{
		Host:         registryNamePrefix + spec.Name,
		Image:        spec.Image,
		ExposureOpts: *exposure,
		Network:      spec.Network,
		Volumes:      spec.Volumes,
		Options: k3d.RegistryOptions{
			DeleteEnabled: spec.DeleteEnabled,
			Proxy: k3d.RegistryProxy{
				RemoteURL: spec.ProxyRemoteURL,
				Username:  spec.ProxyUsername,
				Password:  spec.ProxyPassword,
			},
		},
	}

	if _, err := client.RegistryRun(ctx, runtimes.SelectedRuntime, registry); err != nil {
		cause := fmt.Errorf("create registry %q: %w", spec.Name, err)
		if cleanupErr := DeleteRegistry(context.WithoutCancel(ctx), spec.Name); cleanupErr != nil {
			return nil, errors.Join(cause, cleanupErr)
		}
		return nil, cause
	}
	return GetRegistry(ctx, spec.Name)
}

// GetRegistry reads a registry, returning ErrNotFound only when Docker answered
// and has no registry with that name.
func GetRegistry(ctx context.Context, name string) (*RegistryState, error) {
	node, err := findRegistryNode(ctx, name)
	if err != nil {
		return nil, err
	}

	registry, err := client.RegistryFromNode(node)
	if err != nil {
		return nil, fmt.Errorf("read registry %q: %w", name, err)
	}

	port, err := strconv.Atoi(registry.ExposureOpts.Binding.HostPort)
	if err != nil {
		return nil, fmt.Errorf("parse port of registry %q: %w", name, err)
	}

	image, err := configuredImage(ctx, node.Name)
	if err != nil {
		return nil, err
	}

	return &RegistryState{
		Name:     strings.TrimPrefix(node.Name, registryNamePrefix),
		NodeName: node.Name,
		Ref:      node.Name + ":" + registry.ExposureOpts.Port.Port(),
		Image:    image,
		HostIP:   registry.ExposureOpts.Binding.HostIP,
		Port:     port,
		Networks: node.Networks,
	}, nil
}

// DeleteRegistry removes a registry. Deleting a missing registry is not an error.
func DeleteRegistry(ctx context.Context, name string) error {
	node, err := findRegistryNode(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := client.NodeDelete(ctx, runtimes.SelectedRuntime, node, k3d.NodeDeleteOpts{SkipLBUpdate: true}); err != nil {
		return fmt.Errorf("delete registry %q: %w", name, err)
	}
	return nil
}

func findRegistryNode(ctx context.Context, name string) (*k3d.Node, error) {
	nodes, err := runtimes.SelectedRuntime.GetNodesByLabel(ctx, map[string]string{k3d.LabelRole: string(k3d.RegistryRole)})
	if err != nil {
		return nil, fmt.Errorf("list registries: %w", err)
	}
	for _, node := range nodes {
		if node.Name == registryNamePrefix+name {
			return node, nil
		}
	}
	return nil, fmt.Errorf("registry %q: %w", name, ErrNotFound)
}
