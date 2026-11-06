package k3dclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-connections/nat"
	cliutil "github.com/k3d-io/k3d/v5/cmd/util"
	"github.com/k3d-io/k3d/v5/pkg/client"
	"github.com/k3d-io/k3d/v5/pkg/config"
	configtypes "github.com/k3d-io/k3d/v5/pkg/config/types"
	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	runtimeerrors "github.com/k3d-io/k3d/v5/pkg/runtimes/errors"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	"github.com/k3d-io/k3d/v5/pkg/util"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	loadBalancerFilter = "loadbalancer"
	rollbackTimeout    = 2 * time.Minute
	defaultProtocol    = "tcp"
)

// Port maps a host port through the cluster load balancer. An empty HostIP
// means all interfaces and an empty Protocol means tcp.
type Port struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string
}

// Normalized returns the port with default values in a single canonical form.
func (p Port) Normalized() Port {
	if p.HostIP == "0.0.0.0" || p.HostIP == "::" {
		p.HostIP = ""
	}
	if p.Protocol == "" {
		p.Protocol = defaultProtocol
	}
	return p
}

func (p Port) spec() string {
	n := p.Normalized()
	mapping := fmt.Sprintf("%d:%d/%s", n.HostPort, n.ContainerPort, n.Protocol)
	if n.HostIP != "" {
		return n.HostIP + ":" + mapping
	}
	return mapping
}

// Volume mounts Source from the Docker host at Destination in the selected nodes.
type Volume struct {
	Source      string
	Destination string
	NodeFilters []string
}

// Label is a key and value applied to the selected nodes.
type Label struct {
	Key         string
	Value       string
	NodeFilters []string
}

// EnvVar is an environment variable set on the selected nodes.
type EnvVar struct {
	Name        string
	Value       string
	NodeFilters []string
}

// K3sArg is an extra k3s argument passed to the selected nodes.
type K3sArg struct {
	Arg         string
	NodeFilters []string
}

// HostAlias is an /etc/hosts entry added to the nodes and CoreDNS.
type HostAlias struct {
	IP        string
	Hostnames []string
}

// ClusterSpec is the desired configuration of a cluster.
type ClusterSpec struct {
	Name             string
	Image            string
	Servers          int
	Agents           int
	APIHost          string
	APIHostIP        string
	APIPort          int
	Network          string
	Subnet           string
	Token            string
	Traefik          bool
	Ports            []Port
	Volumes          []Volume
	K3sArgs          []K3sArg
	NodeLabels       []Label
	RuntimeLabels    []Label
	Env              []EnvVar
	HostAliases      []HostAlias
	RegistriesUse    []string
	RegistriesConfig string
	Timeout          time.Duration
}

// Kubeconfig holds cluster credentials, as a full file and as individual fields.
type Kubeconfig struct {
	Raw                  string
	Host                 string
	ClientCertificate    string
	ClientKey            string
	ClusterCACertificate string
}

// ClusterState is what can be read back from a running cluster.
type ClusterState struct {
	Name       string
	Image      string
	Servers    int
	Agents     int
	Network    string
	Token      string
	APIHost    string
	APIHostIP  string
	APIPort    int
	Ports      []Port
	Kubeconfig Kubeconfig
}

// CheckClusterName validates a cluster name without contacting Docker.
func CheckClusterName(name string) error {
	return client.CheckName(name)
}

// BuildSimpleConfig converts a spec to a k3d config. It applies the defaults
// the k3d CLI would otherwise supply and never enables kubeconfig writes.
func BuildSimpleConfig(spec ClusterSpec) conf.SimpleConfig {
	defaults := defaultNodeFilters(spec.Agents)

	cfg := conf.SimpleConfig{
		TypeMeta:     configtypes.TypeMeta{Kind: "Simple", APIVersion: conf.ApiVersion},
		ObjectMeta:   configtypes.ObjectMeta{Name: spec.Name},
		Servers:      spec.Servers,
		Agents:       spec.Agents,
		Image:        spec.Image,
		Network:      spec.Network,
		Subnet:       spec.Subnet,
		ClusterToken: spec.Token,
		ExposeAPI: conf.SimpleExposureOpts{
			Host:     spec.APIHost,
			HostIP:   spec.APIHostIP,
			HostPort: portString(spec.APIPort),
		},
		Options: conf.SimpleConfigOptions{
			K3dOptions: conf.SimpleConfigOptionsK3d{Wait: true, Timeout: spec.Timeout},
		},
		Registries: conf.SimpleConfigRegistries{Use: spec.RegistriesUse},
	}

	if spec.RegistriesConfig != "" {
		cfg.Registries.Config = embeddedRegistriesConfig(spec.RegistriesConfig)
	}

	cfg.Ports = portChanges(spec.Ports, false)

	for _, v := range spec.Volumes {
		cfg.Volumes = append(cfg.Volumes, conf.VolumeWithNodeFilters{
			Volume:      v.Source + ":" + v.Destination,
			NodeFilters: filtersOr(v.NodeFilters, defaults),
		})
	}
	for _, a := range spec.K3sArgs {
		cfg.Options.K3sOptions.ExtraArgs = append(cfg.Options.K3sOptions.ExtraArgs, conf.K3sArgWithNodeFilters{
			Arg:         a.Arg,
			NodeFilters: filtersOr(a.NodeFilters, defaults),
		})
	}
	if !spec.Traefik {
		cfg.Options.K3sOptions.ExtraArgs = append(cfg.Options.K3sOptions.ExtraArgs, conf.K3sArgWithNodeFilters{
			Arg:         "--disable=traefik",
			NodeFilters: []string{"server:*"},
		})
	}
	for _, label := range spec.NodeLabels {
		cfg.Options.K3sOptions.NodeLabels = append(cfg.Options.K3sOptions.NodeLabels, conf.LabelWithNodeFilters{
			Label:       label.Key + "=" + label.Value,
			NodeFilters: filtersOr(label.NodeFilters, defaults),
		})
	}
	for _, label := range spec.RuntimeLabels {
		cfg.Options.Runtime.Labels = append(cfg.Options.Runtime.Labels, conf.LabelWithNodeFilters{
			Label:       label.Key + "=" + label.Value,
			NodeFilters: filtersOr(label.NodeFilters, defaults),
		})
	}
	for _, e := range spec.Env {
		cfg.Env = append(cfg.Env, conf.EnvVarWithNodeFilters{
			EnvVar:      e.Name + "=" + e.Value,
			NodeFilters: filtersOr(e.NodeFilters, defaults),
		})
	}
	for _, a := range spec.HostAliases {
		cfg.HostAliases = append(cfg.HostAliases, k3d.HostAlias{IP: a.IP, Hostnames: a.Hostnames})
	}

	return cfg
}

// embeddedRegistriesConfig guarantees a trailing newline, because k3d treats a
// single-line value as a file path instead of embedded content.
func embeddedRegistriesConfig(content string) string {
	if strings.HasSuffix(content, "\n") {
		return content
	}
	return content + "\n"
}

func defaultNodeFilters(agents int) []string {
	if agents > 0 {
		return []string{"server:*", "agent:*"}
	}
	return []string{"server:*"}
}

func filtersOr(filters, fallback []string) []string {
	if len(filters) > 0 {
		return filters
	}
	return fallback
}

func portString(port int) string {
	if port == 0 {
		return ""
	}
	return strconv.Itoa(port)
}

func portChanges(ports []Port, removal bool) []conf.PortWithNodeFilters {
	changes := make([]conf.PortWithNodeFilters, 0, len(ports))
	for _, p := range ports {
		changes = append(changes, conf.PortWithNodeFilters{
			Port:        p.spec(),
			NodeFilters: []string{loadBalancerFilter},
			Removal:     removal,
		})
	}
	return changes
}

// GetCluster reads a cluster. It returns ErrNotFound only when Docker answered
// and has no nodes for the name. client.ClusterGet alone reports an unreachable
// Docker as "no nodes", which would make Terraform plan to recreate the cluster.
func GetCluster(ctx context.Context, name string) (*ClusterState, error) {
	cluster, err := getRawCluster(ctx, name)
	if err != nil {
		return nil, err
	}

	state := &ClusterState{
		Name:    cluster.Name,
		Network: cluster.Network.Name,
		Token:   cluster.Token,
	}

	for _, node := range cluster.Nodes {
		switch node.Role {
		case k3d.ServerRole:
			state.Servers++
			if state.Image == "" {
				state.Image, err = configuredImage(ctx, node.Name)
				if err != nil {
					return nil, err
				}
			}
			if err := applyAPILabels(state, node); err != nil {
				return nil, err
			}
		case k3d.AgentRole:
			state.Agents++
		case k3d.LoadBalancerRole:
			state.Ports, err = portsFromMap(node.Ports)
			if err != nil {
				return nil, fmt.Errorf("read ports of %s: %w", node.Name, err)
			}
		}
	}

	state.Kubeconfig, err = getKubeconfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func getRawCluster(ctx context.Context, name string) (*k3d.Cluster, error) {
	nodes, err := runtimes.SelectedRuntime.GetNodesByLabel(ctx, map[string]string{k3d.LabelClusterName: name})
	if err != nil {
		return nil, fmt.Errorf("list nodes of cluster %q: %w", name, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("cluster %q: %w", name, ErrNotFound)
	}

	cluster, err := client.ClusterGet(ctx, runtimes.SelectedRuntime, &k3d.Cluster{Name: name})
	if err != nil {
		return nil, fmt.Errorf("get cluster %q: %w", name, err)
	}
	return cluster, nil
}

func applyAPILabels(state *ClusterState, node *k3d.Node) error {
	raw, ok := node.RuntimeLabels[k3d.LabelServerAPIPort]
	if !ok {
		return nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse API port %q on %s: %w", raw, node.Name, err)
	}
	state.APIPort = port
	state.APIHost = node.RuntimeLabels[k3d.LabelServerAPIHost]
	state.APIHostIP = node.RuntimeLabels[k3d.LabelServerAPIHostIP]
	return nil
}

func portsFromMap(portMap nat.PortMap) ([]Port, error) {
	ports := []Port{}
	for port, bindings := range portMap {
		if port.Port() == k3d.DefaultAPIPort && port.Proto() == defaultProtocol {
			continue
		}
		container, err := strconv.Atoi(port.Port())
		if err != nil {
			return nil, fmt.Errorf("parse container port %q: %w", port, err)
		}
		for _, binding := range bindings {
			host, err := strconv.Atoi(binding.HostPort)
			if err != nil {
				return nil, fmt.Errorf("parse host port %q: %w", binding.HostPort, err)
			}
			ports = append(ports, Port{
				HostIP:        binding.HostIP,
				HostPort:      host,
				ContainerPort: container,
				Protocol:      port.Proto(),
			}.Normalized())
		}
	}
	slices.SortFunc(ports, func(a, b Port) int {
		return strings.Compare(a.spec(), b.spec())
	})
	return ports, nil
}

func getKubeconfig(ctx context.Context, cluster *k3d.Cluster) (Kubeconfig, error) {
	kc, err := client.KubeconfigGet(ctx, runtimes.SelectedRuntime, cluster)
	if err != nil {
		return Kubeconfig{}, fmt.Errorf("get kubeconfig of cluster %q: %w", cluster.Name, err)
	}

	raw, err := clientcmd.Write(*kc)
	if err != nil {
		return Kubeconfig{}, fmt.Errorf("serialize kubeconfig of cluster %q: %w", cluster.Name, err)
	}

	context, ok := kc.Contexts[kc.CurrentContext]
	if !ok {
		return Kubeconfig{}, fmt.Errorf("kubeconfig of cluster %q has no context %q", cluster.Name, kc.CurrentContext)
	}
	clusterEntry, ok := kc.Clusters[context.Cluster]
	if !ok {
		return Kubeconfig{}, fmt.Errorf("kubeconfig of cluster %q has no cluster %q", cluster.Name, context.Cluster)
	}
	user, ok := kc.AuthInfos[context.AuthInfo]
	if !ok {
		return Kubeconfig{}, fmt.Errorf("kubeconfig of cluster %q has no user %q", cluster.Name, context.AuthInfo)
	}

	return Kubeconfig{
		Raw:                  string(raw),
		Host:                 clusterEntry.Server,
		ClientCertificate:    string(user.ClientCertificateData),
		ClientKey:            string(user.ClientKeyData),
		ClusterCACertificate: string(clusterEntry.CertificateAuthorityData),
	}, nil
}

// clusterMutations serializes changes to clusters. Terraform runs resources in
// parallel, and two creates could otherwise pick the same free API port between
// choosing it and binding it. Reads stay parallel.
var clusterMutations sync.Mutex

// CreateCluster creates a cluster and returns its state. Any failure after
// containers exist is rolled back, so a retry does not hit "already exists".
func CreateCluster(ctx context.Context, spec ClusterSpec) (*ClusterState, error) {
	clusterMutations.Lock()
	defer clusterMutations.Unlock()

	if _, err := getRawCluster(ctx, spec.Name); err == nil {
		return nil, fmt.Errorf("cluster %q already exists: import it or choose another name", spec.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if spec.APIPort == 0 {
		port, err := cliutil.GetFreePort()
		if err != nil {
			return nil, fmt.Errorf("find a free API port: %w", err)
		}
		spec.APIPort = port
	}

	clusterConfig, err := prepareClusterConfig(ctx, spec)
	if err != nil {
		return nil, err
	}

	if err := client.ClusterRun(ctx, runtimes.SelectedRuntime, clusterConfig); err != nil {
		return nil, rollback(ctx, spec.Name, fmt.Errorf("create cluster %q: %w", spec.Name, err))
	}

	state, err := GetCluster(ctx, spec.Name)
	if err != nil {
		return nil, rollback(ctx, spec.Name, fmt.Errorf("read new cluster %q: %w", spec.Name, err))
	}
	return state, nil
}

func prepareClusterConfig(ctx context.Context, spec ClusterSpec) (*conf.ClusterConfig, error) {
	simple := BuildSimpleConfig(spec)
	if err := config.ProcessSimpleConfig(&simple); err != nil {
		return nil, fmt.Errorf("process config: %w", err)
	}
	clusterConfig, err := config.TransformSimpleToClusterConfig(ctx, runtimes.SelectedRuntime, simple, "")
	if err != nil {
		return nil, fmt.Errorf("transform config: %w", err)
	}
	clusterConfig, err = config.ProcessClusterConfig(*clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("process cluster config: %w", err)
	}
	if err := config.ValidateClusterConfig(ctx, runtimes.SelectedRuntime, *clusterConfig); err != nil {
		return nil, fmt.Errorf("validate cluster config: %w", err)
	}
	return clusterConfig, nil
}

func rollback(ctx context.Context, name string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	if err := deleteCluster(cleanupCtx, name); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback failed, delete the cluster manually: %w", err))
	}
	return fmt.Errorf("%w (changes were rolled back)", cause)
}

// DeleteCluster removes a cluster. Deleting a missing cluster is not an error.
func DeleteCluster(ctx context.Context, name string) error {
	clusterMutations.Lock()
	defer clusterMutations.Unlock()

	return deleteCluster(ctx, name)
}

func deleteCluster(ctx context.Context, name string) error {
	cluster, err := getRawCluster(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := client.ClusterDelete(ctx, runtimes.SelectedRuntime, cluster, k3d.ClusterDeleteOpts{SkipRegistryCheck: true}); err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}
	if cluster.Network.External || cluster.Network.Name == "" {
		return nil
	}
	if err := removeRegistryOnlyNetwork(ctx, cluster.Network.Name); err != nil {
		return fmt.Errorf("delete network of cluster %q: %w", name, err)
	}
	return nil
}

// removeRegistryOnlyNetwork removes a cluster network that only registries still
// use. k3d means to do this, but it matches Docker's "active endpoints" error by
// suffix and Docker 29 appends details, so k3d leaves the network behind. A
// network with any other member is still in use and is left alone.
func removeRegistryOnlyNetwork(ctx context.Context, name string) error {
	network, err := runtimes.SelectedRuntime.GetNetwork(ctx, &k3d.ClusterNetwork{Name: name})
	if errors.Is(err, runtimeerrors.ErrRuntimeNetworkNotExists) {
		return nil
	}
	if err != nil {
		return err
	}

	nodes, err := runtimes.SelectedRuntime.GetNodesInNetwork(ctx, name)
	if err != nil {
		return err
	}
	registries := util.FilterNodesByRole(nodes, k3d.RegistryRole)
	if len(registries) != len(network.Members) {
		return nil
	}

	for _, registry := range registries {
		if err := runtimes.SelectedRuntime.DisconnectNodeFromNetwork(ctx, registry, name); err != nil {
			return err
		}
	}
	return runtimes.SelectedRuntime.DeleteNetwork(ctx, name)
}

// DiffPorts returns the ports to remove and add to get from current to desired.
func DiffPorts(current, desired []Port) (remove, add []Port) {
	normalize := func(ports []Port) []Port {
		out := make([]Port, len(ports))
		for i, p := range ports {
			out[i] = p.Normalized()
		}
		return out
	}
	current, desired = normalize(current), normalize(desired)

	for _, p := range current {
		if !slices.Contains(desired, p) {
			remove = append(remove, p)
		}
	}
	for _, p := range desired {
		if !slices.Contains(current, p) {
			add = append(add, p)
		}
	}
	return remove, add
}

// UpdatePorts applies port changes in place. Removals run first because a port
// may be moving between mappings. Each call replaces the load balancer container.
func UpdatePorts(ctx context.Context, name string, remove, add []Port) error {
	clusterMutations.Lock()
	defer clusterMutations.Unlock()

	if err := editPorts(ctx, name, portChanges(remove, true)); err != nil {
		return fmt.Errorf("remove ports from cluster %q: %w", name, err)
	}
	if err := editPorts(ctx, name, portChanges(add, false)); err != nil {
		return fmt.Errorf("add ports to cluster %q: %w", name, err)
	}
	return nil
}

func editPorts(ctx context.Context, name string, changes []conf.PortWithNodeFilters) error {
	if len(changes) == 0 {
		return nil
	}
	cluster, err := getRawCluster(ctx, name)
	if err != nil {
		return err
	}
	if cluster.ServerLoadBalancer == nil {
		return fmt.Errorf("cluster has no load balancer")
	}
	return client.ClusterEditChangesetSimple(ctx, runtimes.SelectedRuntime, cluster, &conf.SimpleConfig{Ports: changes})
}
