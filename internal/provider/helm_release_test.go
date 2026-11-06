package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestValidateChartSource(t *testing.T) {
	str := types.StringValue
	null := types.StringNull()
	unknown := types.StringUnknown()

	tests := []struct {
		name    string
		model   helmReleaseModel
		wantErr string
	}{
		{name: "local", model: helmReleaseModel{SrcPath: str("/c"), Chart: null, Repository: null, ChartVersion: null, ExpectedChartDigest: null}},
		{name: "local with a digest", model: helmReleaseModel{SrcPath: str("/c"), Chart: null, Repository: null, ChartVersion: null, ExpectedChartDigest: str("x")}},
		{name: "repository", model: helmReleaseModel{SrcPath: null, Chart: str("c"), Repository: str("https://r"), ChartVersion: str("1"), ExpectedChartDigest: null}},
		{name: "oci", model: helmReleaseModel{SrcPath: null, Chart: str("oci://r/c"), Repository: null, ChartVersion: str("1"), ExpectedChartDigest: null}},
		{name: "unknown chart waits for plan", model: helmReleaseModel{SrcPath: null, Chart: unknown, Repository: null, ChartVersion: null, ExpectedChartDigest: null}},
		{
			name:    "local with a repository",
			model:   helmReleaseModel{SrcPath: str("/c"), Chart: null, Repository: str("https://r"), ChartVersion: null, ExpectedChartDigest: null},
			wantErr: "repository cannot be used with src_path",
		},
		{
			name:    "local with a version",
			model:   helmReleaseModel{SrcPath: str("/c"), Chart: null, Repository: null, ChartVersion: str("1"), ExpectedChartDigest: null},
			wantErr: "chart_version cannot be used with src_path",
		},
		{
			name:    "remote with a digest",
			model:   helmReleaseModel{SrcPath: null, Chart: str("oci://r/c"), Repository: null, ChartVersion: str("1"), ExpectedChartDigest: str("x")},
			wantErr: "only applies to src_path",
		},
		{
			name:    "remote without a version",
			model:   helmReleaseModel{SrcPath: null, Chart: str("oci://r/c"), Repository: null, ChartVersion: null, ExpectedChartDigest: null},
			wantErr: "chart_version is required",
		},
		{
			name:    "oci with a repository",
			model:   helmReleaseModel{SrcPath: null, Chart: str("oci://r/c"), Repository: str("https://r"), ChartVersion: str("1"), ExpectedChartDigest: null},
			wantErr: "repository cannot be used with an oci",
		},
		{
			name:    "chart name without a repository",
			model:   helmReleaseModel{SrcPath: null, Chart: str("c"), Repository: null, ChartVersion: str("1"), ExpectedChartDigest: null},
			wantErr: "repository is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diags := validateChartSource(tt.model)
			if tt.wantErr == "" {
				if diags.HasError() {
					t.Fatalf("unexpected: %v", diags)
				}
				return
			}
			if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), tt.wantErr) {
				t.Fatalf("diags = %v, want %q", diags, tt.wantErr)
			}
		})
	}
}

func TestReleaseDrifted(t *testing.T) {
	state := func(status string, revision int64) helmReleaseModel {
		return helmReleaseModel{Status: types.StringValue(status), Revision: types.Int64Value(revision)}
	}

	tests := []struct {
		name    string
		state   helmReleaseModel
		applied string
		want    bool
	}{
		{name: "as applied", state: state("deployed", 2), applied: "2"},
		{name: "upgraded outside terraform", state: state("deployed", 3), applied: "2", want: true},
		{name: "rolled back outside terraform", state: state("deployed", 4), applied: "2", want: true},
		{name: "failed upgrade", state: state("failed", 2), applied: "2", want: true},
		{name: "imported has no applied revision", state: state("deployed", 5)},
		{name: "imported and failed", state: state("failed", 5), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseDrifted(tt.state, tt.applied); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func writeTestChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: demo\nversion: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlanChartDigest(t *testing.T) {
	chart := writeTestChart(t)
	model := func(src, expected types.String) helmReleaseModel {
		return helmReleaseModel{SrcPath: src, ExpectedChartDigest: expected}
	}
	null := types.StringNull()

	t.Run("remote chart has none", func(t *testing.T) {
		got, diags := planChartDigest(model(null, null))
		if diags.HasError() || !got.IsNull() {
			t.Errorf("got %v (%v), want null", got, diags)
		}
	})
	t.Run("unknown path waits", func(t *testing.T) {
		got, diags := planChartDigest(model(types.StringUnknown(), null))
		if diags.HasError() || !got.IsUnknown() {
			t.Errorf("got %v (%v), want unknown", got, diags)
		}
	})

	digest, diags := planChartDigest(model(types.StringValue(chart), null))
	if diags.HasError() || digest.ValueString() == "" {
		t.Fatalf("got %v (%v), want a digest", digest, diags)
	}

	t.Run("matching digest passes", func(t *testing.T) {
		got, diags := planChartDigest(model(types.StringValue(chart), digest))
		if diags.HasError() || !got.Equal(digest) {
			t.Errorf("got %v (%v)", got, diags)
		}
	})
	t.Run("wrong digest fails", func(t *testing.T) {
		_, diags := planChartDigest(model(types.StringValue(chart), types.StringValue("wrong")))
		if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), digest.ValueString()) {
			t.Errorf("diags = %v, want the real digest in the message", diags)
		}
	})
	t.Run("missing path fails", func(t *testing.T) {
		_, diags := planChartDigest(model(types.StringValue(filepath.Join(chart, "missing")), null))
		if !diags.HasError() {
			t.Error("expected an error")
		}
	})
}
