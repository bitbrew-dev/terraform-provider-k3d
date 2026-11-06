package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func testIstioGateway(t *testing.T, gateway istioGatewayModel) types.Object {
	t.Helper()
	object, diags := types.ObjectValueFrom(context.Background(), istioGatewayAttrTypes, gateway)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return object
}

func nullIstioGateway() istioGatewayModel {
	return istioGatewayModel{
		Name:          types.StringNull(),
		ServiceType:   types.StringNull(),
		HTTPNodePort:  types.Int64Null(),
		HTTPSNodePort: types.Int64Null(),
		Values:        types.ListNull(types.StringType),
	}
}

func TestGatewayDefaults(t *testing.T) {
	gateway := nullIstioGateway()

	got := gatewayDefaults(gateway)
	service := got["service"].(map[string]any)
	if service["type"] != "NodePort" {
		t.Errorf("default type = %v", service["type"])
	}
	for _, raw := range service["ports"].([]any) {
		if _, set := raw.(map[string]any)["nodePort"]; set {
			t.Errorf("no nodePort should be set by default: %v", raw)
		}
	}

	gateway.ServiceType = types.StringValue("LoadBalancer")
	gateway.HTTPNodePort = types.Int64Value(31035)
	gateway.HTTPSNodePort = types.Int64Value(30903)
	service = gatewayDefaults(gateway)["service"].(map[string]any)
	if service["type"] != "LoadBalancer" {
		t.Errorf("type = %v", service["type"])
	}

	want := map[string]any{"status-port": nil, "http2": int64(31035), "https": int64(30903)}
	for _, raw := range service["ports"].([]any) {
		port := raw.(map[string]any)
		if port["nodePort"] != want[port["name"].(string)] {
			t.Errorf("%v nodePort = %v", port["name"], port["nodePort"])
		}
	}
}

func TestGatewayRelease(t *testing.T) {
	if got := gatewayRelease(nil); got != "" {
		t.Errorf("no gateway = %q", got)
	}
	gateway := nullIstioGateway()
	if got := gatewayRelease(&gateway); got != defaultGatewayName {
		t.Errorf("default = %q", got)
	}
	gateway.Name = types.StringValue("custom")
	if got := gatewayRelease(&gateway); got != "custom" {
		t.Errorf("custom = %q", got)
	}
}

func TestIstioComponents(t *testing.T) {
	ctx := context.Background()
	list := func(values ...string) types.List {
		l, d := types.ListValueFrom(ctx, types.StringType, values)
		if d.HasError() {
			t.Fatal(d)
		}
		return l
	}
	base := istioModel{
		Namespace:    types.StringValue("mesh"),
		BaseValues:   types.ListNull(types.StringType),
		IstiodValues: list("pilot:\n  replicaCount: 2\n"),
		Gateway:      types.ObjectNull(istioGatewayAttrTypes),
	}

	t.Run("without a gateway", func(t *testing.T) {
		got, diags := istioComponents(ctx, base)
		if diags.HasError() {
			t.Fatal(diags)
		}
		var names []string
		for _, c := range got {
			names = append(names, c.Release)
		}
		if want := []string{"istio-base", "istiod"}; !reflect.DeepEqual(names, want) {
			t.Errorf("order = %v, want %v", names, want)
		}
		global := got[0].Values["global"].(map[string]any)
		if global["istioNamespace"] != "mesh" {
			t.Errorf("namespace = %v", global["istioNamespace"])
		}
		pilot := got[1].Values["pilot"].(map[string]any)
		if pilot["replicaCount"] != float64(2) {
			t.Errorf("istiod values = %v", got[1].Values)
		}
	})

	t.Run("gateway comes last and values override settings", func(t *testing.T) {
		gateway := nullIstioGateway()
		gateway.Values = list("service:\n  type: ClusterIP\n")
		m := base
		m.Gateway = testIstioGateway(t, gateway)

		got, diags := istioComponents(ctx, m)
		if diags.HasError() {
			t.Fatal(diags)
		}
		if len(got) != 3 || got[2].Release != defaultGatewayName || got[2].Chart != "gateway" {
			t.Fatalf("components = %+v", got)
		}
		service := got[2].Values["service"].(map[string]any)
		if service["type"] != "ClusterIP" {
			t.Errorf("type = %v, want the override", service["type"])
		}
	})

	t.Run("invalid values", func(t *testing.T) {
		m := base
		m.IstiodValues = list("a: [")
		if _, diags := istioComponents(ctx, m); !diags.HasError() {
			t.Error("expected an error")
		}
	})
}

func TestIstioDrifted(t *testing.T) {
	state := func(status string, base, istiod int64, gateway types.Int64) istioModel {
		return istioModel{
			Status:          types.StringValue(status),
			BaseRevision:    types.Int64Value(base),
			IstiodRevision:  types.Int64Value(istiod),
			GatewayRevision: gateway,
		}
	}
	applied := func(base, istiod, gateway int64) []byte {
		encoded, err := json.Marshal(istioRevisions{Base: base, Istiod: istiod, Gateway: gateway})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	one := types.Int64Value(1)

	tests := []struct {
		name    string
		state   istioModel
		applied []byte
		want    bool
	}{
		{name: "as applied", state: state("deployed", 1, 1, one), applied: applied(1, 1, 1)},
		{name: "no gateway as applied", state: state("deployed", 1, 1, types.Int64Null()), applied: applied(1, 1, 0)},
		{name: "istiod upgraded by hand", state: state("deployed", 1, 2, one), applied: applied(1, 1, 1), want: true},
		{name: "gateway removed by hand", state: state("missing", 1, 1, types.Int64Value(0)), applied: applied(1, 1, 1), want: true},
		{name: "failed component", state: state("failed", 1, 1, one), applied: applied(1, 1, 1), want: true},
		{name: "imported has nothing applied", state: state("deployed", 3, 4, one)},
		{name: "unreadable record", state: state("deployed", 1, 1, one), applied: []byte("not json"), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := istioDrifted(tt.state, tt.applied); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
