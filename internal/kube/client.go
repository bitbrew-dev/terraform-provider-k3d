// Package kube is the provider's single path to the Kubernetes API. Every
// resource builds an unstructured object and applies it here with server-side
// apply, so retries, error handling, and deletion behave the same everywhere.
package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fluxcd/cli-utils/pkg/kstatus/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	fieldManager = "terraform-provider-k3d"

	// applyRetryWindow covers CRD establishment and webhooks that are still starting.
	applyRetryWindow = 2 * time.Minute
	retryInterval    = 2 * time.Second
	pollInterval     = time.Second
)

// ErrNotFound reports that the API answered and the object does not exist. A
// kind the API no longer serves counts too, since its objects are gone with it.
var ErrNotFound = errors.New("object not found")

// Ref identifies an object.
type Ref struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// String returns a readable name for messages.
func (r Ref) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s %s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name)
}

// RefOf returns the reference of an object.
func RefOf(obj *unstructured.Unstructured) Ref {
	return Ref{
		APIVersion: obj.GetAPIVersion(),
		Kind:       obj.GetKind(),
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}

// Client talks to one cluster.
type Client struct {
	dynamic dynamic.Interface
	mapper  *restmapper.DeferredDiscoveryRESTMapper
}

var clients sync.Map

// New returns a client for a kubeconfig. Clients are cached by the kubeconfig
// content, so resources share API discovery and a replaced cluster, which has a
// new kubeconfig, never reuses a stale client.
func New(kubeconfig string) (*Client, error) {
	if cached, ok := clients.Load(kubeconfig); ok {
		return cached.(*Client), nil
	}

	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}

	client := &Client{
		dynamic: dyn,
		mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc)),
	}
	actual, _ := clients.LoadOrStore(kubeconfig, client)
	return actual.(*Client), nil
}

// resourceFor resolves an object's API resource. Discovery is cached, so a miss
// refreshes it once before reporting a kind as unknown.
func (c *Client) resourceFor(ref Ref) (dynamic.ResourceInterface, string, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, "", fmt.Errorf("parse apiVersion %q: %w", ref.APIVersion, err)
	}
	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}

	mapping, err := c.mapper.RESTMapping(gk, gv.Version)
	if meta.IsNoMatchError(err) {
		c.mapper.Reset()
		mapping, err = c.mapper.RESTMapping(gk, gv.Version)
	}
	if err != nil {
		return nil, "", err
	}

	if mapping.Scope.Name() != meta.RESTScopeNameNamespace {
		return c.dynamic.Resource(mapping.Resource), "", nil
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return c.dynamic.Resource(mapping.Resource).Namespace(namespace), namespace, nil
}

// Apply creates or updates an object with server-side apply. It retries errors
// that clear on their own, which is what lets a custom resource follow its CRD
// in a single apply.
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	deadline := time.Now().Add(applyRetryWindow)
	for {
		result, err := c.applyOnce(ctx, obj)
		if err == nil {
			return result, nil
		}
		if !isRetryable(err) || time.Now().After(deadline) {
			return nil, fmt.Errorf("apply %s: %w", RefOf(obj), err)
		}
		if err := sleep(ctx, retryInterval); err != nil {
			return nil, fmt.Errorf("apply %s: %w", RefOf(obj), err)
		}
	}
}

func (c *Client) applyOnce(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	resource, namespace, err := c.resourceFor(RefOf(obj))
	if err != nil {
		return nil, err
	}
	desired := obj.DeepCopy()
	if namespace != "" {
		desired.SetNamespace(namespace)
	}
	return resource.Apply(ctx, desired.GetName(), desired, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
}

func isRetryable(err error) bool {
	return meta.IsNoMatchError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsInternalError(err) ||
		utilnet.IsConnectionRefused(err) ||
		utilnet.IsConnectionReset(err)
}

// Get reads an object. It returns ErrNotFound only when the API answered, so an
// unreachable API is always a different error.
func (c *Client) Get(ctx context.Context, ref Ref) (*unstructured.Unstructured, error) {
	resource, _, err := c.resourceFor(ref)
	if meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}

	obj, err := resource.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}
	return obj, nil
}

// Exists reports whether an object exists.
func (c *Client) Exists(ctx context.Context, ref Ref) (bool, error) {
	_, err := c.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Delete removes an object and waits until it is really gone, because objects
// with finalizers linger and re-creating the same name fails until they leave.
func (c *Client) Delete(ctx context.Context, ref Ref, timeout time.Duration) error {
	resource, _, err := c.resourceFor(ref)
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}

	propagation := metav1.DeletePropagationForeground
	err = resource.Delete(ctx, ref.Name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", ref, err)
	}

	return poll(ctx, timeout, fmt.Sprintf("%s to be deleted", ref), func() (bool, error) {
		exists, err := c.Exists(ctx, ref)
		return !exists, err
	})
}

// WaitReady waits until an object reports it has reached its desired state,
// using the same status rules as Helm's wait.
func (c *Client) WaitReady(ctx context.Context, ref Ref, timeout time.Duration) error {
	return poll(ctx, timeout, fmt.Sprintf("%s to be ready", ref), func() (bool, error) {
		obj, err := c.Get(ctx, ref)
		if err != nil {
			return false, err
		}
		result, err := status.Compute(obj)
		if err != nil {
			return false, fmt.Errorf("compute status of %s: %w", ref, err)
		}
		if result.Status == status.FailedStatus {
			return false, fmt.Errorf("%s failed: %s", ref, result.Message)
		}
		return result.Status == status.CurrentStatus, nil
	})
}

// WaitFor waits until done reports true for an object, and returns the last
// version it saw, so a caller can report its status after a failure too.
func (c *Client) WaitFor(ctx context.Context, ref Ref, timeout time.Duration, description string, done func(*unstructured.Unstructured) (bool, error)) (*unstructured.Unstructured, error) {
	var latest *unstructured.Unstructured
	err := poll(ctx, timeout, fmt.Sprintf("%s %s", ref, description), func() (bool, error) {
		obj, err := c.Get(ctx, ref)
		if err != nil {
			return false, err
		}
		latest = obj
		return done(obj)
	})
	return latest, err
}

// WaitJob waits for a Job to finish and returns an error describing any failure.
func (c *Client) WaitJob(ctx context.Context, namespace, name string, timeout time.Duration) error {
	ref := Ref{APIVersion: "batch/v1", Kind: "Job", Namespace: namespace, Name: name}
	return poll(ctx, timeout, fmt.Sprintf("%s to finish", ref), func() (bool, error) {
		obj, err := c.Get(ctx, ref)
		if err != nil {
			return false, err
		}
		done, failure := jobOutcome(obj)
		if failure != "" {
			return false, fmt.Errorf("%s failed: %s%s", ref, failure, c.podFailures(ctx, namespace, name))
		}
		return done, nil
	})
}

func jobOutcome(job *unstructured.Unstructured) (done bool, failure string) {
	conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["status"] != "True" {
			continue
		}
		switch condition["type"] {
		case "Complete":
			return true, ""
		case "Failed":
			return false, fmt.Sprintf("%v: %v", condition["reason"], condition["message"])
		}
	}
	return false, ""
}

// podFailures describes why a Job's containers stopped. It is best effort,
// because the Job's own failure is already being reported.
func (c *Client) podFailures(ctx context.Context, namespace, job string) string {
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	list, err := c.dynamic.Resource(pods).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil {
		return ""
	}
	var details []string
	for _, pod := range list.Items {
		details = append(details, terminatedContainers(&pod)...)
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, "; ") + ")"
}

func terminatedContainers(pod *unstructured.Unstructured) []string {
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	var details []string
	for _, raw := range statuses {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		reason, found, _ := unstructured.NestedString(container, "state", "terminated", "reason")
		if !found {
			continue
		}
		exitCode, _, _ := unstructured.NestedInt64(container, "state", "terminated", "exitCode")
		detail := fmt.Sprintf("%s/%v: %s, exit code %d", pod.GetName(), container["name"], reason, exitCode)
		if message, _, _ := unstructured.NestedString(container, "state", "terminated", "message"); message != "" {
			detail += ": " + strings.TrimSpace(message)
		}
		details = append(details, detail)
	}
	return details
}

func poll(ctx context.Context, timeout time.Duration, description string, done func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		finished, err := done()
		if err != nil && !isRetryable(err) {
			return err
		}
		if err == nil && finished {
			return nil
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, description)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
