package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/k3dclient"
	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	defaultNamespace   = "default"
	defaultWaitTimeout = "5m"
	deleteTimeout      = 5 * time.Minute
)

// errClusterGone reports that a resource's cluster no longer exists, so the
// object is gone with it. An unreachable Docker or API is a different error.
var errClusterGone = errors.New("cluster not found")

func clusterAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "Cluster to manage the object in, usually `k3d_cluster.<name>.id`, so the object is recreated when the cluster is replaced. Exactly one of `cluster` or `kubeconfig` is required.",
		Optional:            true,
		PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
}

func kubeconfigAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "Kubeconfig contents, for a cluster that is reached directly and not through Docker. Not recorded as a dependency on the cluster's lifetime, so prefer `cluster` for clusters in the same configuration. It is stored in Terraform state, so use an encrypted state backend.",
		Optional:            true,
		Sensitive:           true,
	}
}

func metadataMapAttribute(description string) schema.MapAttribute {
	return schema.MapAttribute{
		MarkdownDescription: description + " Only keys set here are tracked, so ones added by controllers do not show as changes.",
		ElementType:         types.StringType,
		Optional:            true,
	}
}

func requiredReplaceString(description string, validators ...validator.String) schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: description,
		Required:            true,
		Validators:          validators,
		PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
}

func namespaceAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "Namespace of the object. Defaults to `default`.",
		Optional:            true,
		Computed:            true,
		Default:             stringdefault.StaticString(defaultNamespace),
		PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
}

func connectionValidators() []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("cluster"), path.MatchRoot("kubeconfig")),
	}
}

// connect returns a client for a resource. It runs only at apply and refresh,
// never while planning a new object, so an object can share an apply with the
// cluster it lives in.
func connect(ctx context.Context, cluster, kubeconfig types.String) (*kube.Client, error) {
	raw, err := resolveKubeconfig(ctx, cluster, kubeconfig)
	if err != nil {
		return nil, err
	}
	return kube.New(raw)
}

func resolveKubeconfig(ctx context.Context, cluster, kubeconfig types.String) (string, error) {
	if cluster.IsNull() {
		return kubeconfig.ValueString(), nil
	}
	raw, err := k3dclient.GetKubeconfig(ctx, cluster.ValueString())
	if errors.Is(err, k3dclient.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", errClusterGone, cluster.ValueString())
	}
	return raw, err
}

// isGone reports whether an error means the object no longer exists, either on
// its own or along with its cluster.
func isGone(err error) bool {
	return errors.Is(err, errClusterGone) || errors.Is(err, kube.ErrNotFound)
}

func newObject(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return obj
}

func stringMap(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m.IsNull() || m.IsUnknown() {
		return nil, diags
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out, diags
}

// setMetadata adds labels and annotations. Leaving a key out of server-side
// apply removes it if this provider set it before, and leaves others' keys alone.
func setMetadata(ctx context.Context, obj *unstructured.Unstructured, labels, annotations types.Map) diag.Diagnostics {
	l, diags := stringMap(ctx, labels)
	a, d := stringMap(ctx, annotations)
	diags.Append(d...)
	if len(l) > 0 {
		obj.SetLabels(l)
	}
	if len(a) > 0 {
		obj.SetAnnotations(a)
	}
	return diags
}

// trackedMap refreshes the keys already in prior from the live values. Keys the
// live object lacks drop out, which shows as a change that restores them, and
// keys set by others never enter state.
func trackedMap(ctx context.Context, prior types.Map, live map[string]string) (types.Map, diag.Diagnostics) {
	if prior.IsNull() || prior.IsUnknown() {
		return prior, nil
	}
	tracked, diags := stringMap(ctx, prior)
	refreshed := make(map[string]string, len(tracked))
	for key := range tracked {
		if value, ok := live[key]; ok {
			refreshed[key] = value
		}
	}
	out, d := types.MapValueFrom(ctx, types.StringType, refreshed)
	diags.Append(d...)
	return out, diags
}

// objectID builds an ID that also works as the import ID.
func objectID(cluster types.String, parts ...string) string {
	return strings.Join(append([]string{cluster.ValueString()}, parts...), "/")
}

// splitImportID splits an import ID into exactly want parts.
func splitImportID(id string, format string, want int) ([]string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != want || slices.Contains(parts, "") {
		return nil, fmt.Errorf("invalid import ID %q: want %s", id, format)
	}
	return parts, nil
}

func parseTimeout(value types.String, fallback string) (time.Duration, error) {
	if value.IsNull() || value.IsUnknown() {
		return time.ParseDuration(fallback)
	}
	return time.ParseDuration(value.ValueString())
}
