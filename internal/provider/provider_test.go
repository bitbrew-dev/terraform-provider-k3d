package provider

import (
	"context"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
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

func TestSchemaIsValid(t *testing.T) {
	diags := providerSchema(t).ValidateImplementation(context.Background())
	if diags.HasError() {
		t.Fatalf("invalid schema: %v", diags)
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

	attr, ok := providerSchema(t).Attributes["docker_host"].(schema.StringAttribute)
	if !ok {
		t.Fatal("docker_host is not a string attribute")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp validator.StringResponse
			for _, v := range attr.Validators {
				v.ValidateString(context.Background(), validator.StringRequest{ConfigValue: tt.value}, &resp)
			}
			if got := resp.Diagnostics.HasError(); got != tt.wantErr {
				t.Errorf("HasError = %v, want %v: %v", got, tt.wantErr, resp.Diagnostics)
			}
		})
	}
}

func TestConfigure(t *testing.T) {
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
		{name: "empty means client default", value: nil, want: ""},
		{name: "unknown is an error", value: tftypes.UnknownValue, env: fromEnv, wantErr: true},
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
			if tt.wantErr {
				return
			}

			for name, data := range map[string]any{"resource": resp.ResourceData, "data source": resp.DataSourceData} {
				cfg, ok := data.(*Config)
				if !ok {
					t.Fatalf("%s data has type %T, want *Config", name, data)
				}
				if cfg.DockerHost != tt.want {
					t.Errorf("%s DockerHost = %q, want %q", name, cfg.DockerHost, tt.want)
				}
			}
		})
	}
}
