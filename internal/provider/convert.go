package provider

import "github.com/hashicorp/terraform-plugin-framework/types/basetypes"

func objectAsOptions() basetypes.ObjectAsOptions {
	return basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true}
}
