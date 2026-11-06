package helm

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testChartYAML = "apiVersion: v2\nname: demo\nversion: 1.0.0\n"

func writeChart(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func digest(t *testing.T, path string) string {
	t.Helper()
	got, err := LocalDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLocalDigest(t *testing.T) {
	base := map[string]string{
		"Chart.yaml":       testChartYAML,
		"values.yaml":      "a: 1\n",
		"templates/a.yaml": "kind: A\n",
		".helmignore":      "*.md\nignored/\n",
	}
	original := digest(t, writeChart(t, base))

	with := func(name, content string) map[string]string {
		files := map[string]string{}
		for k, v := range base {
			files[k] = v
		}
		files[name] = content
		return files
	}

	t.Run("same content gives the same digest", func(t *testing.T) {
		if got := digest(t, writeChart(t, base)); got != original {
			t.Errorf("got %s, want %s", got, original)
		}
	})

	changes := []struct {
		name    string
		file    string
		content string
		same    bool
	}{
		{name: "template changed", file: "templates/a.yaml", content: "kind: B\n"},
		{name: "values changed", file: "values.yaml", content: "a: 2\n"},
		{name: "file added", file: "templates/b.yaml", content: "kind: B\n"},
		{name: "ignored file", file: "README.md", content: "docs\n", same: true},
		{name: "ignored folder", file: "ignored/x.yaml", content: "x\n", same: true},
		{name: "hidden template", file: "templates/.hidden", content: "x\n", same: true},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			got := digest(t, writeChart(t, with(tt.file, tt.content)))
			if (got == original) != tt.same {
				t.Errorf("digest same = %v, want %v", got == original, tt.same)
			}
		})
	}

	t.Run("moving content between files changes it", func(t *testing.T) {
		one := digest(t, writeChart(t, map[string]string{"Chart.yaml": testChartYAML, "a": "xy", "b": ""}))
		two := digest(t, writeChart(t, map[string]string{"Chart.yaml": testChartYAML, "a": "x", "b": "y"}))
		if one == two {
			t.Error("digests collide")
		}
	})
}

func TestLocalDigestMissingPath(t *testing.T) {
	if _, err := LocalDigest(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error")
	}
}

func TestCheckLocal(t *testing.T) {
	const withDependency = "apiVersion: v2\nname: demo\nversion: 1.0.0\ndependencies:\n- name: sub\n  version: 1.0.0\n  repository: https://example.com\n"
	const subchart = "apiVersion: v2\nname: sub\nversion: 1.0.0\n"
	const lock = "dependencies:\n- name: sub\n  version: 1.0.0\n  repository: https://example.com\ndigest: sha256:abc\ngenerated: \"2026-01-01T00:00:00Z\"\n"

	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{name: "plain chart", files: map[string]string{"Chart.yaml": testChartYAML}},
		{
			name: "dependency locked and built",
			files: map[string]string{
				"Chart.yaml":            withDependency,
				"Chart.lock":            lock,
				"charts/sub/Chart.yaml": subchart,
			},
		},
		{
			name: "dependency without a lock",
			files: map[string]string{
				"Chart.yaml":            withDependency,
				"charts/sub/Chart.yaml": subchart,
			},
			wantErr: "no Chart.lock",
		},
		{
			name:    "dependency locked but not built",
			files:   map[string]string{"Chart.yaml": withDependency, "Chart.lock": lock},
			wantErr: "helm dependency build",
		},
		{name: "not a chart", files: map[string]string{"values.yaml": "a: 1\n"}, wantErr: "load chart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLocal(writeChart(t, tt.files))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckLocalMissingPath(t *testing.T) {
	if err := CheckLocal(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error")
	}
}

func TestSourceLoadLocal(t *testing.T) {
	path := writeChart(t, map[string]string{"Chart.yaml": testChartYAML})
	ch, err := Source{Path: path}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ch == nil {
		t.Error("expected a chart")
	}
}

func TestIsolatedSettingsKeepRegistryLogin(t *testing.T) {
	t.Setenv("HELM_REGISTRY_CONFIG", "/custom/registry.json")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	settings, err := isolatedSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.RegistryConfig != "/custom/registry.json" {
		t.Errorf("registry config = %q", settings.RegistryConfig)
	}
	for name, path := range map[string]string{
		"repository cache":  settings.RepositoryCache,
		"repository config": settings.RepositoryConfig,
		"content cache":     settings.ContentCache,
	} {
		if !strings.Contains(path, "terraform-provider-k3d") {
			t.Errorf("%s = %q is not isolated", name, path)
		}
	}
}

func TestMergeValues(t *testing.T) {
	tests := []struct {
		name      string
		documents []string
		set       map[string]string
		want      map[string]any
		wantErr   bool
	}{
		{name: "nothing", want: map[string]any{}},
		{
			name:      "later documents win and nested maps merge",
			documents: []string{"a: 1\nb:\n  c: 1\n  d: 1\n", "a: 2\nb:\n  c: 2\n"},
			want:      map[string]any{"a": float64(2), "b": map[string]any{"c": float64(2), "d": float64(1)}},
		},
		{
			name:      "a scalar replaces a map",
			documents: []string{"a:\n  b: 1\n", "a: 2\n"},
			want:      map[string]any{"a": float64(2)},
		},
		{
			name:      "set wins over documents",
			documents: []string{"image:\n  tag: old\n"},
			set:       map[string]string{"image.tag": "new"},
			want:      map[string]any{"image": map[string]any{"tag": "new"}},
		},
		{
			name: "set keeps commas in a value",
			set:  map[string]string{"list": "a,b"},
			want: map[string]any{"list": "a,b"},
		},
		{name: "an empty document", documents: []string{""}, want: map[string]any{}},
		{name: "invalid yaml", documents: []string{"a: ["}, wantErr: true},
		{name: "a document that is not a map", documents: []string{"- a\n"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergeValues(tt.documents, tt.set)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestMergeValuesSetTypes(t *testing.T) {
	got, err := MergeValues(nil, map[string]string{"replicas": "3", "enabled": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got["replicas"] != int64(3) || got["enabled"] != true {
		t.Errorf("got %#v, want Helm's --set typing", got)
	}
}
