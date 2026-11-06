package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// importedKey marks a resource whose create-only settings have never been
// recorded. k3d cannot report them back, so after an import they are null in
// state even though the real object may already match the configuration.
const importedKey = "imported"

var (
	importedTrue  = []byte("true")
	importedFalse = []byte("false")
)

type privateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

func isImported(ctx context.Context, private privateReader) (bool, diag.Diagnostics) {
	value, diags := private.GetKey(ctx, importedKey)
	return string(value) == string(importedTrue), diags
}

func markImported(ctx context.Context, private privateWriter) diag.Diagnostics {
	return private.SetKey(ctx, importedKey, importedTrue)
}

func clearImported(ctx context.Context, private privateWriter) diag.Diagnostics {
	return private.SetKey(ctx, importedKey, importedFalse)
}

// createOnly is an attribute that can only change by replacing the resource.
type createOnly struct {
	name  string
	state attr.Value
	plan  attr.Value
}

// replacementPaths returns the attributes whose change forces a replacement. On
// an imported resource a null state value is adopted rather than replaced, so
// importing a cluster and matching its config never destroys it.
func replacementPaths(attrs []createOnly, imported bool) path.Paths {
	var paths path.Paths
	for _, a := range attrs {
		if a.plan.IsUnknown() || a.state.Equal(a.plan) {
			continue
		}
		if imported && a.state.IsNull() {
			continue
		}
		paths = append(paths, path.Root(a.name))
	}
	return paths
}
