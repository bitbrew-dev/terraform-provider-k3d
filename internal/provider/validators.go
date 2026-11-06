package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

type stringCheck struct {
	description string
	check       func(string) error
}

// Description returns a plain text description of the check.
func (v stringCheck) Description(context.Context) string { return v.description }

// MarkdownDescription returns a markdown description of the check.
func (v stringCheck) MarkdownDescription(context.Context) string { return v.description }

// ValidateString runs the check on known values.
func (v stringCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := v.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid value", err.Error())
	}
}

func pinnedImage() validator.String {
	return stringCheck{description: "must be an image with a pinned tag or digest", check: checkPinnedImage}
}

func clusterName() validator.String {
	return stringCheck{description: "must be a valid k3d cluster name", check: k3dclient.CheckClusterName}
}

func duration() validator.String {
	return stringCheck{description: "must be a positive duration such as 10m", check: checkDuration}
}

func checkPinnedImage(image string) error {
	if strings.Contains(image, "@sha256:") {
		return nil
	}
	lastSegment := image[strings.LastIndex(image, "/")+1:]
	_, tag, found := strings.Cut(lastSegment, ":")
	if !found || tag == "" {
		return fmt.Errorf("image %q has no tag: pin a version such as rancher/k3s:v1.32.4-k3s1", image)
	}
	if tag == "latest" || tag == "stable" {
		return fmt.Errorf("image %q uses a moving tag: pin a version so the cluster is reproducible", image)
	}
	return nil
}

func checkDuration(value string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	if d <= 0 {
		return fmt.Errorf("duration %q must be positive", value)
	}
	return nil
}
