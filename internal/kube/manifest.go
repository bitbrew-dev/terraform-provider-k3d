package kube

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// ParseManifest parses YAML or JSON holding exactly one object. It needs no
// cluster, so it is safe to call during plan.
func ParseManifest(manifest string) (*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)

	var found *unstructured.Unstructured
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		if found != nil {
			return nil, errors.New("manifest holds more than one object: use one k3d_manifest per object, with for_each if needed")
		}
		found = &unstructured.Unstructured{Object: doc}
	}

	if found == nil {
		return nil, errors.New("manifest is empty")
	}
	if found.GetAPIVersion() == "" {
		return nil, errors.New("manifest has no apiVersion")
	}
	if found.GetKind() == "" {
		return nil, errors.New("manifest has no kind")
	}
	if strings.HasSuffix(found.GetKind(), "List") {
		return nil, fmt.Errorf("manifest kind %s is a list: use one k3d_manifest per object", found.GetKind())
	}
	if found.GetName() == "" {
		return nil, errors.New("manifest has no metadata.name, and generateName is not supported")
	}
	return found, nil
}
