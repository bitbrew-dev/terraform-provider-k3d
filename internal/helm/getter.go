package helm

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// restClientGetter gives Helm a cluster connection from a kubeconfig in memory.
type restClientGetter struct {
	loader clientcmd.ClientConfig
}

func newRESTClientGetter(kubeconfig, namespace string) (*restClientGetter, error) {
	raw, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	current, ok := raw.Contexts[raw.CurrentContext]
	if !ok {
		return nil, fmt.Errorf("kubeconfig has no context %q", raw.CurrentContext)
	}
	overrides := &clientcmd.ConfigOverrides{Context: *current}
	overrides.Context.Namespace = namespace

	return &restClientGetter{
		loader: clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, overrides, nil),
	}, nil
}

// ToRESTConfig returns the connection settings.
func (g *restClientGetter) ToRESTConfig() (*rest.Config, error) {
	return g.loader.ClientConfig()
}

// ToDiscoveryClient returns a cached discovery client.
func (g *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	config, err := g.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	client, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(client), nil
}

// ToRESTMapper returns a mapper from kinds to API resources.
func (g *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	client, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(client)
	return restmapper.NewShortcutExpander(mapper, client, nil), nil
}

// ToRawKubeConfigLoader returns the kubeconfig loader.
func (g *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return g.loader
}
