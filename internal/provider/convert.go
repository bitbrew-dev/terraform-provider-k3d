package provider

import (
	"errors"
	"maps"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

func objectAsOptions() basetypes.ObjectAsOptions {
	return basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true}
}

func diagsError(diags diag.Diagnostics) error {
	var errs []error
	for _, d := range diags.Errors() {
		errs = append(errs, errors.New(d.Summary()+": "+d.Detail()))
	}
	return errors.Join(errs...)
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
