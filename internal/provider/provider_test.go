package provider

import (
	"context"
	"os"
	"testing"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func providerSchema(t *testing.T) schema.Schema {
	t.Helper()

	var resp fwprovider.SchemaResponse
	New("test")().Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func TestSchemasAreValid(t *testing.T) {
	ctx := context.Background()

	if diags := providerSchema(t).ValidateImplementation(ctx); diags.HasError() {
		t.Fatalf("provider schema: %v", diags)
	}

	p := New("test")()
	for _, newResource := range p.Resources(ctx) {
		res := newResource()
		var meta resource.MetadataResponse
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: typeName}, &meta)
		var resp resource.SchemaResponse
		res.Schema(ctx, resource.SchemaRequest{}, &resp)
		if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("resource %s: %v", meta.TypeName, diags)
		}
	}
	for _, newDataSource := range p.DataSources(ctx) {
		ds := newDataSource()
		var meta datasource.MetadataResponse
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: typeName}, &meta)
		var resp datasource.SchemaResponse
		ds.Schema(ctx, datasource.SchemaRequest{}, &resp)
		if diags := resp.Schema.ValidateImplementation(ctx); diags.HasError() {
			t.Errorf("data source %s: %v", meta.TypeName, diags)
		}
	}
}

func TestMetadata(t *testing.T) {
	var resp fwprovider.MetadataResponse
	New("1.2.3")().Metadata(context.Background(), fwprovider.MetadataRequest{}, &resp)

	if resp.TypeName != "k3d" {
		t.Errorf("TypeName = %q, want k3d", resp.TypeName)
	}
	if resp.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", resp.Version)
	}
}

func TestDockerHostValidation(t *testing.T) {
	tests := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{name: "unix socket", value: types.StringValue("unix:///var/run/docker.sock")},
		{name: "tcp", value: types.StringValue("tcp://10.0.0.1:2376")},
		{name: "ssh", value: types.StringValue("ssh://user@host")},
		{name: "null is allowed", value: types.StringNull()},
		{name: "unknown is allowed", value: types.StringUnknown()},
		{name: "no scheme", value: types.StringValue("10.0.0.1:2376"), wantErr: true},
		{name: "unsupported scheme", value: types.StringValue("http://host"), wantErr: true},
		{name: "scheme without address", value: types.StringValue("tcp://"), wantErr: true},
		{name: "empty", value: types.StringValue(""), wantErr: true},
	}

	schemaAttr, ok := providerSchema(t).Attributes["docker_host"].(schema.StringAttribute)
	if !ok {
		t.Fatal("docker_host is not a string attribute")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp validator.StringResponse
			for _, v := range schemaAttr.Validators {
				v.ValidateString(context.Background(), validator.StringRequest{ConfigValue: tt.value}, &resp)
			}
			if got := resp.Diagnostics.HasError(); got != tt.wantErr {
				t.Errorf("HasError = %v, want %v: %v", got, tt.wantErr, resp.Diagnostics)
			}
		})
	}
}

func TestConfigureSetsDockerHost(t *testing.T) {
	const (
		fromConfig = "tcp://config:2376"
		fromEnv    = "tcp://env:2376"
	)

	tests := []struct {
		name    string
		value   any
		env     string
		want    string
		wantErr bool
	}{
		{name: "config wins over env", value: fromConfig, env: fromEnv, want: fromConfig},
		{name: "env is the fallback", value: nil, env: fromEnv, want: fromEnv},
		{name: "unset stays unset", value: nil, want: ""},
		{name: "unknown is an error", value: tftypes.UnknownValue, env: fromEnv, want: fromEnv, wantErr: true},
	}

	schemaDef := providerSchema(t)
	objectType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"docker_host": tftypes.String}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(dockerHostEnv, tt.env)

			req := fwprovider.ConfigureRequest{
				Config: tfsdk.Config{
					Schema: schemaDef,
					Raw: tftypes.NewValue(objectType, map[string]tftypes.Value{
						"docker_host": tftypes.NewValue(tftypes.String, tt.value),
					}),
				},
			}

			var resp fwprovider.ConfigureResponse
			New("test")().Configure(context.Background(), req, &resp)

			if got := resp.Diagnostics.HasError(); got != tt.wantErr {
				t.Fatalf("HasError = %v, want %v: %v", got, tt.wantErr, resp.Diagnostics)
			}
			if got := os.Getenv(dockerHostEnv); got != tt.want {
				t.Errorf("%s = %q, want %q", dockerHostEnv, got, tt.want)
			}
		})
	}
}

func TestCheckPinnedImage(t *testing.T) {
	tests := []struct {
		image   string
		wantErr bool
	}{
		{image: "rancher/k3s:v1.32.4-k3s1"},
		{image: "docker.io/rancher/k3s:v1.32.4-k3s1"},
		{image: "localhost:5000/k3s:v1"},
		{image: "rancher/k3s@sha256:abc"},
		{image: "rancher/k3s", wantErr: true},
		{image: "rancher/k3s:latest", wantErr: true},
		{image: "rancher/k3s:", wantErr: true},
		{image: "localhost:5000/k3s", wantErr: true},
		{image: "latest", wantErr: true},
		{image: "stable", wantErr: true},
		{image: "+v1.32", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if err := checkPinnedImage(tt.image); (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckDuration(t *testing.T) {
	tests := []struct {
		value   string
		wantErr bool
	}{
		{value: "10m"},
		{value: "90s"},
		{value: "0s", wantErr: true},
		{value: "-1m", wantErr: true},
		{value: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if err := checkDuration(tt.value); (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReplacementPaths(t *testing.T) {
	str := func(s string) attr.Value { return types.StringValue(s) }
	null := types.StringNull()
	unknown := types.StringUnknown()

	tests := []struct {
		name     string
		state    attr.Value
		plan     attr.Value
		imported bool
		want     int
	}{
		{name: "unchanged", state: str("a"), plan: str("a")},
		{name: "changed", state: str("a"), plan: str("b"), want: 1},
		{name: "added", state: null, plan: str("a"), want: 1},
		{name: "removed", state: str("a"), plan: null, want: 1},
		{name: "unknown is ignored", state: str("a"), plan: unknown},
		{name: "imported adopts a null state value", state: null, plan: str("a"), imported: true},
		{name: "imported still replaces on a real difference", state: str("a"), plan: str("b"), imported: true, want: 1},
		{name: "imported still replaces when removed", state: str("a"), plan: null, imported: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replacementPaths([]createOnly{{"x", tt.state, tt.plan}}, tt.imported)
			if len(got) != tt.want {
				t.Errorf("got %v, want %d paths", got, tt.want)
			}
		})
	}
}

func TestValidatePorts(t *testing.T) {
	port := func(container int64, protocol types.String) portModel {
		return portModel{ContainerPort: types.Int64Value(container), Protocol: protocol}
	}
	tcp, udp, unset := types.StringValue("tcp"), types.StringValue("udp"), types.StringNull()

	tests := []struct {
		name    string
		ports   []portModel
		wantErr bool
	}{
		{name: "distinct", ports: []portModel{port(80, tcp), port(81, tcp)}},
		{name: "same port on both protocols", ports: []portModel{port(53, tcp), port(53, udp)}},
		{name: "duplicate", ports: []portModel{port(80, tcp), port(80, tcp)}, wantErr: true},
		{name: "unset protocol means tcp", ports: []portModel{port(80, tcp), port(80, unset)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validatePorts(tt.ports).HasError(); got != tt.wantErr {
				t.Errorf("HasError = %v, want %v", got, tt.wantErr)
			}
		})
	}
}

func TestPortsToSetKeepsPriorForm(t *testing.T) {
	ctx := context.Background()
	elemType := types.ObjectType{AttrTypes: portAttrTypes}

	explicit := portModel{
		HostIP:        types.StringValue("0.0.0.0"),
		HostPort:      types.Int64Value(8080),
		ContainerPort: types.Int64Value(80),
		Protocol:      types.StringValue("tcp"),
	}
	prior, diags := types.SetValueFrom(ctx, elemType, []portModel{explicit})
	if diags.HasError() {
		t.Fatal(diags)
	}

	observed := []k3dclient.Port{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}
	got, diags := portsToSet(ctx, observed, prior)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if !got.Equal(prior) {
		t.Errorf("got %v, want prior form %v", got, prior)
	}
}

func TestPortsToSetEmpty(t *testing.T) {
	ctx := context.Background()
	elemType := types.ObjectType{AttrTypes: portAttrTypes}

	got, diags := portsToSet(ctx, nil, types.SetNull(elemType))
	if diags.HasError() || !got.IsNull() {
		t.Errorf("null prior with no ports = %v, want null (%v)", got, diags)
	}

	empty, _ := types.SetValueFrom(ctx, elemType, []portModel{})
	got, diags = portsToSet(ctx, nil, empty)
	if diags.HasError() || got.IsNull() || len(got.Elements()) != 0 {
		t.Errorf("empty prior with no ports = %v, want empty (%v)", got, diags)
	}
}

func TestPortsToSetReportsDriftInCanonicalForm(t *testing.T) {
	ctx := context.Background()
	elemType := types.ObjectType{AttrTypes: portAttrTypes}

	got, diags := portsToSet(ctx, []k3dclient.Port{{HostIP: "0.0.0.0", HostPort: 9090, ContainerPort: 90, Protocol: "tcp"}}, types.SetNull(elemType))
	if diags.HasError() {
		t.Fatal(diags)
	}

	var models []portModel
	if diags := got.ElementsAs(ctx, &models, false); diags.HasError() {
		t.Fatal(diags)
	}
	if len(models) != 1 || !models[0].HostIP.IsNull() || !models[0].Protocol.IsNull() {
		t.Errorf("got %+v, want defaults as null", models)
	}
}
