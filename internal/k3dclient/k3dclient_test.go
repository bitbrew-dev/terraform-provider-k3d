package k3dclient

import (
	"context"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/go-connections/nat"
	conf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

func TestLibraryVersionMatchesGoMod(t *testing.T) {
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`github\.com/k3d-io/k3d/v5 (v\S+)`).FindSubmatch(goMod)
	if match == nil {
		t.Fatal("k3d not found in go.mod")
	}
	if got := string(match[1]); got != LibraryVersion {
		t.Errorf("go.mod has k3d %s, LibraryVersion is %s", got, LibraryVersion)
	}
}

func TestHelperImagesArePinned(t *testing.T) {
	t.Setenv(k3d.K3dEnvImageHelperTag, "")
	t.Setenv(k3d.K3dEnvImageLoadbalancer, "")
	t.Setenv(k3d.K3dEnvImageTools, "")

	want := strings.TrimPrefix(LibraryVersion, "v")
	for name, image := range map[string]string{
		"load balancer": k3d.GetLoadbalancerImage(),
		"tools":         k3d.GetToolsImage(),
	} {
		if !strings.HasSuffix(image, ":"+want) {
			t.Errorf("%s image = %s, want tag %s", name, image, want)
		}
	}
}

func TestUnreachableDockerIsNotNotFound(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

	ctx := context.Background()
	reads := map[string]func() error{
		"GetCluster":  func() error { _, err := GetCluster(ctx, "any"); return err },
		"GetRegistry": func() error { _, err := GetRegistry(ctx, "any"); return err },
		"DeleteCluster": func() error {
			return DeleteCluster(ctx, "any")
		},
		"DeleteRegistry": func() error {
			return DeleteRegistry(ctx, "any")
		},
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			err := read()
			if err == nil {
				t.Fatal("expected an error when Docker is unreachable")
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("unreachable Docker reported as not found: %v", err)
			}
		})
	}
}

func TestPortNormalized(t *testing.T) {
	tests := []struct {
		name string
		in   Port
		want Port
	}{
		{"defaults", Port{HostPort: 80, ContainerPort: 8080}, Port{HostPort: 80, ContainerPort: 8080, Protocol: "tcp"}},
		{"wildcard ipv4", Port{HostIP: "0.0.0.0", HostPort: 80, ContainerPort: 8080, Protocol: "tcp"}, Port{HostPort: 80, ContainerPort: 8080, Protocol: "tcp"}},
		{"wildcard ipv6", Port{HostIP: "::", HostPort: 80, ContainerPort: 8080, Protocol: "udp"}, Port{HostPort: 80, ContainerPort: 8080, Protocol: "udp"}},
		{"specific ip stays", Port{HostIP: "127.0.0.1", HostPort: 80, ContainerPort: 8080}, Port{HostIP: "127.0.0.1", HostPort: 80, ContainerPort: 8080, Protocol: "tcp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Normalized(); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPortSpec(t *testing.T) {
	tests := []struct {
		in   Port
		want string
	}{
		{Port{HostPort: 8080, ContainerPort: 80}, "8080:80/tcp"},
		{Port{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "udp"}, "127.0.0.1:8080:80/udp"},
		{Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80}, "8080:80/tcp"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.in.spec(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPortsFromMap(t *testing.T) {
	got, err := portsFromMap(nat.PortMap{
		"6443/tcp":  {{HostIP: "0.0.0.0", HostPort: "6445"}},
		"80/tcp":    {{HostIP: "0.0.0.0", HostPort: "8080"}},
		"30090/udp": {{HostIP: "127.0.0.1", HostPort: "11800"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []Port{
		{HostIP: "127.0.0.1", HostPort: 11800, ContainerPort: 30090, Protocol: "udp"},
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPortsFromMapKeepsAPIPortOnUDP(t *testing.T) {
	got, err := portsFromMap(nat.PortMap{"6443/udp": {{HostPort: "7000"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %+v, want the udp mapping kept", got)
	}
}

func TestDiffPorts(t *testing.T) {
	a := Port{HostPort: 80, ContainerPort: 8080}
	b := Port{HostPort: 81, ContainerPort: 8081}
	c := Port{HostPort: 82, ContainerPort: 8082}

	tests := []struct {
		name             string
		current, desired []Port
		remove, add      []Port
	}{
		{name: "no change", current: []Port{a}, desired: []Port{a}},
		{name: "add", current: []Port{a}, desired: []Port{a, b}, add: []Port{b.Normalized()}},
		{name: "remove", current: []Port{a, b}, desired: []Port{a}, remove: []Port{b.Normalized()}},
		{name: "swap", current: []Port{a, b}, desired: []Port{a, c}, remove: []Port{b.Normalized()}, add: []Port{c.Normalized()}},
		{
			name:    "explicit defaults are not a change",
			current: []Port{{HostIP: "0.0.0.0", HostPort: 80, ContainerPort: 8080, Protocol: "tcp"}},
			desired: []Port{a},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remove, add := DiffPorts(tt.current, tt.desired)
			if !reflect.DeepEqual(remove, tt.remove) {
				t.Errorf("remove = %+v, want %+v", remove, tt.remove)
			}
			if !reflect.DeepEqual(add, tt.add) {
				t.Errorf("add = %+v, want %+v", add, tt.add)
			}
		})
	}
}

func TestBuildSimpleConfigDefaults(t *testing.T) {
	cfg := BuildSimpleConfig(ClusterSpec{Name: "c", Image: "i:1", Servers: 1, Traefik: true})

	if !cfg.Options.K3dOptions.Wait {
		t.Error("Wait must be true, the kubeconfig needs a running API")
	}
	if cfg.Options.KubeconfigOptions != (conf.SimpleConfigOptionsKubeconfig{}) {
		t.Errorf("kubeconfig writes must stay off, got %+v", cfg.Options.KubeconfigOptions)
	}
	if len(cfg.Options.K3sOptions.ExtraArgs) != 0 {
		t.Errorf("unexpected k3s args: %+v", cfg.Options.K3sOptions.ExtraArgs)
	}
}

func TestBuildSimpleConfigTraefikDisabled(t *testing.T) {
	cfg := BuildSimpleConfig(ClusterSpec{Name: "c", Servers: 2, Agents: 1})

	want := []conf.K3sArgWithNodeFilters{{Arg: "--disable=traefik", NodeFilters: []string{"server:*"}}}
	if !reflect.DeepEqual(cfg.Options.K3sOptions.ExtraArgs, want) {
		t.Errorf("got %+v, want %+v", cfg.Options.K3sOptions.ExtraArgs, want)
	}
}

func TestBuildSimpleConfigNodeFilters(t *testing.T) {
	explicit := []string{"agent:0"}

	tests := []struct {
		name   string
		agents int
		want   []string
	}{
		{"servers only", 0, []string{"server:*"}},
		{"with agents", 2, []string{"server:*", "agent:*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := BuildSimpleConfig(ClusterSpec{
				Agents:        tt.agents,
				Traefik:       true,
				Volumes:       []Volume{{Source: "/a", Destination: "/b"}, {Source: "/c", Destination: "/d", NodeFilters: explicit}},
				K3sArgs:       []K3sArg{{Arg: "--x"}},
				NodeLabels:    []Label{{Key: "k", Value: "v"}},
				RuntimeLabels: []Label{{Key: "k", Value: "v"}},
				Env:           []EnvVar{{Name: "N", Value: "V"}},
			})

			if got := cfg.Volumes[0]; got.Volume != "/a:/b" || !reflect.DeepEqual(got.NodeFilters, tt.want) {
				t.Errorf("volume = %+v, want filters %v", got, tt.want)
			}
			if got := cfg.Volumes[1].NodeFilters; !reflect.DeepEqual(got, explicit) {
				t.Errorf("explicit filters = %v, want %v", got, explicit)
			}
			if got := cfg.Options.K3sOptions.ExtraArgs[0].NodeFilters; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("k3s arg filters = %v, want %v", got, tt.want)
			}
			if got := cfg.Options.K3sOptions.NodeLabels[0]; got.Label != "k=v" || !reflect.DeepEqual(got.NodeFilters, tt.want) {
				t.Errorf("node label = %+v", got)
			}
			if got := cfg.Options.Runtime.Labels[0]; got.Label != "k=v" || !reflect.DeepEqual(got.NodeFilters, tt.want) {
				t.Errorf("runtime label = %+v", got)
			}
			if got := cfg.Env[0]; got.EnvVar != "N=V" || !reflect.DeepEqual(got.NodeFilters, tt.want) {
				t.Errorf("env = %+v", got)
			}
		})
	}
}

func TestBuildSimpleConfigPortsUseLoadBalancer(t *testing.T) {
	cfg := BuildSimpleConfig(ClusterSpec{Traefik: true, Ports: []Port{{HostPort: 8080, ContainerPort: 80}}})

	want := []conf.PortWithNodeFilters{{Port: "8080:80/tcp", NodeFilters: []string{"loadbalancer"}}}
	if !reflect.DeepEqual(cfg.Ports, want) {
		t.Errorf("got %+v, want %+v", cfg.Ports, want)
	}
}

func TestBuildSimpleConfigAPI(t *testing.T) {
	cfg := BuildSimpleConfig(ClusterSpec{Traefik: true, APIHost: "h", APIHostIP: "127.0.0.1", APIPort: 6445})

	want := conf.SimpleExposureOpts{Host: "h", HostIP: "127.0.0.1", HostPort: "6445"}
	if cfg.ExposeAPI != want {
		t.Errorf("got %+v, want %+v", cfg.ExposeAPI, want)
	}
}

func TestEmbeddedRegistriesConfigIsNeverAFilePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"mirrors: {}", "mirrors: {}\n"},
		{"mirrors: {}\n", "mirrors: {}\n"},
	}
	for _, tt := range tests {
		cfg := BuildSimpleConfig(ClusterSpec{Traefik: true, RegistriesConfig: tt.in})
		if cfg.Registries.Config != tt.want {
			t.Errorf("config for %q = %q, want %q", tt.in, cfg.Registries.Config, tt.want)
		}
	}
}

func TestCheckClusterName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"coldbrew", false},
		{"tfacc-abc123", false},
		{"", true},
		{"has space", true},
		{"-leading", true},
		{strings.Repeat("a", 33), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckClusterName(tt.name); (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
