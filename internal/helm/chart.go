package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/ignore"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/strvals"
	"sigs.k8s.io/yaml"
)

const lockFile = "Chart.lock"

// locateMutex guards Helm's repository cache, which concurrent downloads share.
var locateMutex sync.Mutex

// Source says where a chart comes from: a local Path, or a Chart name or oci://
// reference, with a Repository for classic repositories.
type Source struct {
	Path       string
	Repository string
	Chart      string
	Version    string
	PlainHTTP  bool
}

// CheckLocal validates a chart folder without contacting a cluster or the
// network, so a missing path or unbuilt dependencies fail during plan.
func CheckLocal(path string) error {
	ch, err := loader.LoadDir(path)
	if err != nil {
		return fmt.Errorf("load chart %s: %w", path, err)
	}
	return checkDependencies(ch, path)
}

func checkDependencies(ch chart.Charter, path string) error {
	accessor, err := chart.NewAccessor(ch)
	if err != nil {
		return fmt.Errorf("read chart: %w", err)
	}
	dependencies := accessor.MetaDependencies()
	if len(dependencies) == 0 {
		return nil
	}

	if path != "" {
		if _, err := os.Stat(filepath.Join(path, lockFile)); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("chart %s has dependencies but no %s, so its versions are not pinned: run helm dependency update", path, lockFile)
		}
	}
	if err := action.CheckDependencies(ch, dependencies); err != nil {
		return fmt.Errorf("%w: run helm dependency build", err)
	}
	return nil
}

// LocalDigest hashes every file Helm would load from a chart folder, honoring
// .helmignore, so that any change to the chart changes the digest.
func LocalDigest(path string) (string, error) {
	rules, err := ignoreRules(path)
	if err != nil {
		return "", err
	}

	var files []string
	err = filepath.WalkDir(path, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if rules.Ignore(filepath.ToSlash(relative), info) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			files = append(files, relative)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("read chart %s: %w", path, err)
	}
	sort.Strings(files)

	hash := sha256.New()
	for _, relative := range files {
		content, err := os.ReadFile(filepath.Join(path, relative))
		if err != nil {
			return "", fmt.Errorf("read chart file %s: %w", relative, err)
		}
		hash.Write([]byte(fmt.Sprintf("%s\x00%d\x00", filepath.ToSlash(relative), len(content))))
		hash.Write(content)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ignoreRules(path string) (*ignore.Rules, error) {
	rules := ignore.Empty()
	file := filepath.Join(path, ignore.HelmIgnore)
	if _, err := os.Stat(file); err == nil {
		rules, err = ignore.ParseFile(file)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
	}
	rules.AddDefaults()
	return rules, nil
}

// Load returns the chart a source points to, downloading it if it is remote.
func (s Source) Load() (chart.Charter, error) {
	if s.Path != "" {
		ch, err := loader.LoadDir(s.Path)
		if err != nil {
			return nil, fmt.Errorf("load chart %s: %w", s.Path, err)
		}
		if err := checkDependencies(ch, s.Path); err != nil {
			return nil, err
		}
		return ch, nil
	}

	path, err := s.download()
	if err != nil {
		return nil, fmt.Errorf("download chart %s: %w", s.Chart, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", s.Chart, err)
	}
	if err := checkDependencies(ch, ""); err != nil {
		return nil, err
	}
	return ch, nil
}

func (s Source) download() (string, error) {
	settings, err := isolatedSettings()
	if err != nil {
		return "", err
	}

	options := []registry.ClientOption{
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
		registry.ClientOptWriter(io.Discard),
	}
	if s.PlainHTTP {
		options = append(options, registry.ClientOptPlainHTTP())
	}
	client, err := registry.NewClient(options...)
	if err != nil {
		return "", fmt.Errorf("create registry client: %w", err)
	}

	install := action.NewInstall(&action.Configuration{RegistryClient: client})
	install.RepoURL = s.Repository
	install.Version = s.Version
	install.PlainHTTP = s.PlainHTTP

	locateMutex.Lock()
	defer locateMutex.Unlock()
	return install.LocateChart(s.Chart, settings)
}

// isolatedSettings keeps downloads out of the user's Helm repositories and
// cache. The registry config stays at its default, so credentials from
// helm registry login still work for private charts.
func isolatedSettings() (*cli.EnvSettings, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("find cache directory: %w", err)
	}
	root := filepath.Join(cache, "terraform-provider-k3d", "helm")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}

	settings := cli.New()
	settings.RepositoryCache = filepath.Join(root, "repository")
	settings.RepositoryConfig = filepath.Join(root, "repositories.yaml")
	settings.ContentCache = filepath.Join(root, "content")
	return settings, nil
}

// MergeValues combines YAML documents in order, with later ones winning, then
// applies set values, which use Helm's --set syntax and type rules.
func MergeValues(documents []string, set map[string]string) (map[string]any, error) {
	merged := map[string]any{}
	for i, document := range documents {
		var values map[string]any
		if err := yaml.Unmarshal([]byte(document), &values); err != nil {
			return nil, fmt.Errorf("parse values document %d: %w", i+1, err)
		}
		merged = mergeMaps(merged, values)
	}

	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		escaped := strings.ReplaceAll(set[key], ",", `\,`)
		if err := strvals.ParseInto(key+"="+escaped, merged); err != nil {
			return nil, fmt.Errorf("parse set value %q: %w", key, err)
		}
	}
	return merged, nil
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	for key, value := range overlay {
		if nested, ok := value.(map[string]any); ok {
			if existing, ok := base[key].(map[string]any); ok {
				base[key] = mergeMaps(existing, nested)
				continue
			}
		}
		base[key] = value
	}
	return base
}
