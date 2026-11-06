package kube

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// Context describes the current context of a kubeconfig.
type Context struct {
	Name        string
	ClusterName string
	UserName    string
	Namespace   string
	Host        string
}

// CurrentContext reads the current context from a kubeconfig's contents. It
// needs no cluster connection.
func CurrentContext(kubeconfig string) (Context, error) {
	raw, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return Context{}, fmt.Errorf("parse kubeconfig: %w", err)
	}
	current, ok := raw.Contexts[raw.CurrentContext]
	if !ok {
		return Context{}, fmt.Errorf("kubeconfig has no context %q", raw.CurrentContext)
	}
	cluster, ok := raw.Clusters[current.Cluster]
	if !ok {
		return Context{}, fmt.Errorf("kubeconfig has no cluster %q", current.Cluster)
	}
	return Context{
		Name:        raw.CurrentContext,
		ClusterName: current.Cluster,
		UserName:    current.AuthInfo,
		Namespace:   current.Namespace,
		Host:        cluster.Server,
	}, nil
}
