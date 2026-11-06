package kube

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantErr  string
		wantRef  Ref
	}{
		{
			name:     "namespaced yaml",
			manifest: "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: a\n  namespace: b\n",
			wantRef:  Ref{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "b", Name: "a"},
		},
		{
			name:     "json from jsonencode",
			manifest: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`,
			wantRef:  Ref{APIVersion: "v1", Kind: "ConfigMap", Name: "a"},
		},
		{
			name:     "separators around one object",
			manifest: "---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n---\n",
			wantRef:  Ref{APIVersion: "v1", Kind: "Namespace", Name: "a"},
		},
		{
			name:     "two objects",
			manifest: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: b\n",
			wantErr:  "more than one object",
		},
		{name: "empty", manifest: "", wantErr: "empty"},
		{name: "only separators", manifest: "---\n---\n", wantErr: "empty"},
		{name: "no apiVersion", manifest: "kind: Namespace\nmetadata:\n  name: a\n", wantErr: "no apiVersion"},
		{name: "no kind", manifest: "apiVersion: v1\nmetadata:\n  name: a\n", wantErr: "no kind"},
		{name: "no name", manifest: "apiVersion: v1\nkind: Namespace\nmetadata: {}\n", wantErr: "no metadata.name"},
		{name: "generate name", manifest: "apiVersion: v1\nkind: Pod\nmetadata:\n  generateName: a-\n", wantErr: "generateName"},
		{name: "list", manifest: "apiVersion: v1\nkind: List\nmetadata:\n  name: a\nitems: []\n", wantErr: "is a list"},
		{name: "invalid yaml", manifest: "a: [", wantErr: "parse manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, err := ParseManifest(tt.manifest)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := RefOf(obj); got != tt.wantRef {
				t.Errorf("ref = %+v, want %+v", got, tt.wantRef)
			}
		})
	}
}

func TestJobOutcome(t *testing.T) {
	job := func(conditions ...map[string]any) *unstructured.Unstructured {
		items := make([]any, len(conditions))
		for i, c := range conditions {
			items[i] = c
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": items}}}
	}

	tests := []struct {
		name        string
		job         *unstructured.Unstructured
		wantDone    bool
		wantFailure string
	}{
		{name: "no status", job: &unstructured.Unstructured{Object: map[string]any{}}},
		{name: "running", job: job(map[string]any{"type": "Suspended", "status": "False"})},
		{name: "complete", job: job(map[string]any{"type": "Complete", "status": "True"}), wantDone: true},
		{name: "complete but false", job: job(map[string]any{"type": "Complete", "status": "False"})},
		{
			name:        "failed",
			job:         job(map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "message": "too many"}),
			wantFailure: "BackoffLimitExceeded: too many",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, failure := jobOutcome(tt.job)
			if done != tt.wantDone || failure != tt.wantFailure {
				t.Errorf("got (%v, %q), want (%v, %q)", done, failure, tt.wantDone, tt.wantFailure)
			}
		})
	}
}

func TestTerminatedContainers(t *testing.T) {
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p"},
		"status": map[string]any{"containerStatuses": []any{
			map[string]any{"name": "running", "state": map[string]any{"running": map[string]any{}}},
			map[string]any{"name": "main", "state": map[string]any{"terminated": map[string]any{
				"reason": "Error", "exitCode": int64(3), "message": "boom\n",
			}}},
		}},
	}}

	got := terminatedContainers(pod)
	if len(got) != 1 || got[0] != "p/main: Error, exit code 3: boom" {
		t.Errorf("got %q", got)
	}
}

func TestIsRetryable(t *testing.T) {
	gr := schema.GroupResource{Resource: "things"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"kind not served yet", &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "Thing"}}, true},
		{"wrapped kind not served", fmt.Errorf("x: %w", &meta.NoKindMatchError{}), true},
		{"unavailable", apierrors.NewServiceUnavailable("starting"), true},
		{"webhook not ready", apierrors.NewInternalError(errors.New("failed calling webhook")), true},
		{"too many requests", apierrors.NewTooManyRequests("slow", 1), true},
		{"not found", apierrors.NewNotFound(gr, "a"), false},
		{"forbidden", apierrors.NewForbidden(gr, "a", errors.New("no")), false},
		{"invalid", apierrors.NewBadRequest("bad"), false},
		{"plain", errors.New("plain"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefString(t *testing.T) {
	if got := (Ref{Kind: "Namespace", Name: "a"}).String(); got != "Namespace a" {
		t.Errorf("got %q", got)
	}
	if got := (Ref{Kind: "Secret", Namespace: "n", Name: "a"}).String(); got != "Secret n/a" {
		t.Errorf("got %q", got)
	}
}

func TestNewRejectsBadKubeconfig(t *testing.T) {
	if _, err := New("not: [a kubeconfig"); err == nil {
		t.Error("expected an error")
	}
}

func TestNewCachesByContent(t *testing.T) {
	const kubeconfig = `
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
	first, err := New(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("same kubeconfig should share a client")
	}

	other, err := New(strings.ReplaceAll(kubeconfig, "127.0.0.1:1", "127.0.0.1:2"))
	if err != nil {
		t.Fatal(err)
	}
	if first == other {
		t.Error("a different kubeconfig must not share a client")
	}
}

func TestCurrentContext(t *testing.T) {
	const kubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: k3d-demo
  cluster:
    server: https://0.0.0.0:6445
contexts:
- name: k3d-demo
  context:
    cluster: k3d-demo
    user: admin@k3d-demo
    namespace: app
current-context: k3d-demo
users:
- name: admin@k3d-demo
  user: {}
`
	got, err := CurrentContext(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	want := Context{Name: "k3d-demo", ClusterName: "k3d-demo", UserName: "admin@k3d-demo", Namespace: "app", Host: "https://0.0.0.0:6445"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	for name, bad := range map[string]string{
		"invalid":            "not: [yaml",
		"no current context": "apiVersion: v1\nkind: Config\n",
		"missing cluster":    strings.ReplaceAll(kubeconfig, "cluster: k3d-demo", "cluster: other"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CurrentContext(bad); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
