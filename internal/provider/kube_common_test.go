package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testMap(t *testing.T, values map[string]string) types.Map {
	t.Helper()
	m, diags := types.MapValueFrom(context.Background(), types.StringType, values)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return m
}

func TestTrackedMap(t *testing.T) {
	ctx := context.Background()
	live := map[string]string{"mine": "changed", "kubernetes.io/metadata.name": "added-by-kubernetes"}

	t.Run("only tracked keys are refreshed", func(t *testing.T) {
		got, diags := trackedMap(ctx, testMap(t, map[string]string{"mine": "old"}), live)
		if diags.HasError() {
			t.Fatal(diags)
		}
		if want := testMap(t, map[string]string{"mine": "changed"}); !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("a removed key drops out", func(t *testing.T) {
		got, diags := trackedMap(ctx, testMap(t, map[string]string{"gone": "v"}), live)
		if diags.HasError() {
			t.Fatal(diags)
		}
		if want := testMap(t, map[string]string{}); !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("null stays null", func(t *testing.T) {
		got, diags := trackedMap(ctx, types.MapNull(types.StringType), live)
		if diags.HasError() || !got.IsNull() {
			t.Errorf("got %v (%v), want null", got, diags)
		}
	})
}

func TestSplitImportID(t *testing.T) {
	tests := []struct {
		id      string
		want    []string
		wantErr bool
	}{
		{id: "c/n/a", want: []string{"c", "n", "a"}},
		{id: "c/a", wantErr: true},
		{id: "c/n/a/x", wantErr: true},
		{id: "c//a", wantErr: true},
		{id: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got, err := splitImportID(tt.id, "<cluster>/<namespace>/<name>", 3)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func secretFor(t *testing.T, data map[string]string) secretModel {
	t.Helper()
	m := secretModel{
		Data:          types.MapNull(types.StringType),
		DataWOVersion: types.Int64Null(),
	}
	if data != nil {
		m.Data = testMap(t, data)
	}
	return m
}

func TestPlanDataHash(t *testing.T) {
	ctx := context.Background()
	hash := func(m secretModel) types.String {
		t.Helper()
		got, diags := planDataHash(ctx, m)
		if diags.HasError() {
			t.Fatal(diags)
		}
		return got
	}

	base := hash(secretFor(t, map[string]string{"a": "1", "b": "2"}))

	t.Run("key order does not matter", func(t *testing.T) {
		if got := hash(secretFor(t, map[string]string{"b": "2", "a": "1"})); !got.Equal(base) {
			t.Errorf("got %v, want %v", got, base)
		}
	})
	t.Run("a new value changes it", func(t *testing.T) {
		if got := hash(secretFor(t, map[string]string{"a": "1", "b": "3"})); got.Equal(base) {
			t.Error("hash did not change")
		}
	})
	t.Run("unknown data is unknown", func(t *testing.T) {
		m := secretFor(t, nil)
		m.Data = types.MapUnknown(types.StringType)
		if !hash(m).IsUnknown() {
			t.Error("want unknown")
		}
	})
	t.Run("an unknown value is unknown", func(t *testing.T) {
		m := secretFor(t, nil)
		value, diags := types.MapValue(types.StringType, map[string]attr.Value{"a": types.StringUnknown()})
		if diags.HasError() {
			t.Fatal(diags)
		}
		m.Data = value
		if !hash(m).IsUnknown() {
			t.Error("want unknown")
		}
	})
	t.Run("write-only hashes only the version", func(t *testing.T) {
		one := secretFor(t, nil)
		one.DataWOVersion = types.Int64Value(1)
		two := secretFor(t, nil)
		two.DataWOVersion = types.Int64Value(2)

		if hash(one).Equal(hash(two)) {
			t.Error("a new version must change the hash")
		}
		if want := hashString("version:1"); hash(one).ValueString() != want {
			t.Errorf("got %v, want the hash of the version alone", hash(one))
		}
	})
}

func TestBuildSecretEncodesData(t *testing.T) {
	m := secretModel{
		Namespace:   types.StringValue("n"),
		Name:        types.StringValue("s"),
		Type:        types.StringValue("Opaque"),
		Labels:      types.MapNull(types.StringType),
		Annotations: types.MapNull(types.StringType),
	}
	obj, err := buildSecret(context.Background(), m, map[string]string{"k": "value"})
	if err != nil {
		t.Fatal(err)
	}

	got, _, _ := unstructured.NestedString(obj.Object, "data", "k")
	if got != "dmFsdWU=" {
		t.Errorf("data = %q, want base64", got)
	}

	decoded, err := decodeSecretData(obj)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["k"] != "value" {
		t.Errorf("round trip = %q", decoded["k"])
	}
}

func TestBuildJob(t *testing.T) {
	ctx := context.Background()
	list := func(values ...string) types.List {
		l, diags := types.ListValueFrom(ctx, types.StringType, values)
		if diags.HasError() {
			t.Fatal(diags)
		}
		return l
	}
	nullList := types.ListNull(types.StringType)
	nullMap := types.MapNull(types.StringType)

	m := jobModel{
		Namespace:        types.StringValue("n"),
		Name:             types.StringValue("j"),
		Labels:           nullMap,
		Annotations:      nullMap,
		PodAnnotations:   testMap(t, map[string]string{"sidecar.istio.io/inject": "false"}),
		Image:            types.StringValue("busybox:1.36"),
		Command:          list("sh", "-c"),
		Args:             list("true"),
		Env:              testMap(t, map[string]string{"B": "2", "A": "1"}),
		ImagePullSecrets: list("ghcr"),
		BackoffLimit:     types.Int64Value(2),
		TTLSeconds:       types.Int64Value(60),
	}

	obj, diags := buildJob(ctx, m)
	if diags.HasError() {
		t.Fatal(diags)
	}

	backoff, _, _ := unstructured.NestedInt64(obj.Object, "spec", "backoffLimit")
	ttl, _, _ := unstructured.NestedInt64(obj.Object, "spec", "ttlSecondsAfterFinished")
	policy, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "spec", "restartPolicy")
	annotation, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", "sidecar.istio.io/inject")
	if backoff != 2 || ttl != 60 || policy != "Never" || annotation != "false" {
		t.Errorf("backoff=%d ttl=%d policy=%q annotation=%q", backoff, ttl, policy, annotation)
	}

	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]any)
	wantEnv := []any{map[string]any{"name": "A", "value": "1"}, map[string]any{"name": "B", "value": "2"}}
	if !reflect.DeepEqual(container["env"], wantEnv) {
		t.Errorf("env = %v, want sorted %v", container["env"], wantEnv)
	}

	m.Command, m.Args, m.ImagePullSecrets = nullList, nullList, nullList
	m.Env, m.PodAnnotations, m.TTLSeconds = nullMap, nullMap, types.Int64Null()
	minimal, diags := buildJob(ctx, m)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if _, found, _ := unstructured.NestedInt64(minimal.Object, "spec", "ttlSecondsAfterFinished"); found {
		t.Error("ttl should be absent when unset")
	}
}

func TestIdentityChanged(t *testing.T) {
	state := manifestModel{
		APIVersion: types.StringValue("networking.istio.io/v1beta1"),
		Kind:       types.StringValue("Gateway"),
		Namespace:  types.StringValue("istio-system"),
		Name:       types.StringValue("g"),
	}
	object := func(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
		return newObject(apiVersion, kind, namespace, name)
	}

	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{"same", object("networking.istio.io/v1beta1", "Gateway", "istio-system", "g"), false},
		{"new version of the same API", object("networking.istio.io/v1", "Gateway", "istio-system", "g"), false},
		{"namespace left out", object("networking.istio.io/v1", "Gateway", "", "g"), false},
		{"different group", object("gateway.networking.k8s.io/v1", "Gateway", "istio-system", "g"), true},
		{"different kind", object("networking.istio.io/v1", "Sidecar", "istio-system", "g"), true},
		{"different name", object("networking.istio.io/v1", "Gateway", "istio-system", "h"), true},
		{"different namespace", object("networking.istio.io/v1", "Gateway", "other", "g"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := identityChanged(state, tt.obj); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObjectID(t *testing.T) {
	got := objectID(types.StringValue("c"), "n", "a")
	if !strings.EqualFold(got, "c/n/a") {
		t.Errorf("got %q", got)
	}
}
