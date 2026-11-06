package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestChartSourceRefusesAChartChangedAfterPlan(t *testing.T) {
	chart := writeTestChart(t)
	digest, diags := planChartDigest(helmReleaseModel{SrcPath: types.StringValue(chart), ExpectedChartDigest: types.StringNull()})
	if diags.HasError() {
		t.Fatal(diags)
	}
	model := helmReleaseModel{SrcPath: types.StringValue(chart), ChartDigest: digest}

	if _, err := chartSource(model); err != nil {
		t.Fatalf("unchanged chart: %v", err)
	}

	if err := os.WriteFile(filepath.Join(chart, "values.yaml"), []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := chartSource(model); err == nil || !strings.Contains(err.Error(), "changed after the plan") {
		t.Errorf("err = %v, want a changed-after-plan error", err)
	}
}

func TestPrepareReleaseMergesValuesInOrder(t *testing.T) {
	ctx := context.Background()
	chart := writeTestChart(t)
	digest, diags := planChartDigest(helmReleaseModel{SrcPath: types.StringValue(chart), ExpectedChartDigest: types.StringNull()})
	if diags.HasError() {
		t.Fatal(diags)
	}
	list := func(values ...string) types.List {
		l, d := types.ListValueFrom(ctx, types.StringType, values)
		if d.HasError() {
			t.Fatal(d)
		}
		return l
	}

	plan := helmReleaseModel{
		Name:        types.StringValue("demo"),
		Namespace:   types.StringValue("apps"),
		SrcPath:     types.StringValue(chart),
		ChartDigest: digest,
		Timeout:     types.StringValue("1m"),
		Values:      list("a: from-values\nb: from-values\nc: from-values\n"),
		Set:         testMap(t, map[string]string{"c": "from-set"}),
		Wait:        types.BoolValue(true),
		MaxHistory:  types.Int64Value(3),
	}
	config := helmReleaseModel{ValuesWO: list("b: from-write-only\nc: from-write-only\n")}

	ch, values, options, diags := prepareRelease(ctx, plan, config)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if ch == nil {
		t.Error("expected a chart")
	}
	want := map[string]any{"a": "from-values", "b": "from-write-only", "c": "from-set"}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
	if options.Name != "demo" || options.Namespace != "apps" || !options.Wait || options.MaxHistory != 3 || options.Timeout != time.Minute {
		t.Errorf("options = %+v", options)
	}
}
