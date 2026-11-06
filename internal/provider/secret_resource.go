package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/bitbrew-dev/terraform-provider-k3d/internal/kube"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
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

const defaultSecretType = "Opaque"

var (
	_ resource.Resource                     = (*secretResource)(nil)
	_ resource.ResourceWithImportState      = (*secretResource)(nil)
	_ resource.ResourceWithConfigValidators = (*secretResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*secretResource)(nil)
)

// NewSecretResource returns the k3d_secret resource.
func NewSecretResource() resource.Resource {
	return &secretResource{}
}

type secretResource struct{}

type secretModel struct {
	ID            types.String `tfsdk:"id"`
	Cluster       types.String `tfsdk:"cluster"`
	Kubeconfig    types.String `tfsdk:"kubeconfig"`
	Name          types.String `tfsdk:"name"`
	Namespace     types.String `tfsdk:"namespace"`
	Labels        types.Map    `tfsdk:"labels"`
	Annotations   types.Map    `tfsdk:"annotations"`
	Type          types.String `tfsdk:"type"`
	Data          types.Map    `tfsdk:"data"`
	DataWO        types.Map    `tfsdk:"data_wo"`
	DataWOVersion types.Int64  `tfsdk:"data_wo_version"`
	DataHash      types.String `tfsdk:"data_hash"`
}

func secretRef(namespace, name string) kube.Ref {
	return kube.Ref{APIVersion: "v1", Kind: "Secret", Namespace: namespace, Name: name}
}

func (r *secretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Kubernetes Secret. Set `data` to keep values in Terraform state with change detection, " +
			"or `data_wo` with `data_wo_version` to keep them out of state. Creating one that already exists fails: import it instead. " +
			"For a pull secret, set `type = \"kubernetes.io/dockerconfigjson\"` and `.dockerconfigjson = jsonencode({ auths = { ... } })`. " +
			"Import with `<cluster>/<namespace>/<name>`.",
		Attributes: map[string]schema.Attribute{
			"id":          computedString("`<cluster>/<namespace>/<name>`.", false),
			"cluster":     clusterAttribute(),
			"kubeconfig":  kubeconfigAttribute(),
			"name":        requiredReplaceString("Secret name."),
			"namespace":   namespaceAttribute(),
			"labels":      metadataMapAttribute("Labels, such as `argocd.argoproj.io/secret-type`."),
			"annotations": metadataMapAttribute("Annotations."),
			"type": schema.StringAttribute{
				MarkdownDescription: "Secret type. Defaults to `Opaque`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(defaultSecretType),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"data": schema.MapAttribute{
				MarkdownDescription: "Keys and plain text values, stored in Terraform state. Only keys set here are tracked.",
				ElementType:         types.StringType,
				Optional:            true,
				Sensitive:           true,
			},
			"data_wo": schema.MapAttribute{
				MarkdownDescription: "Keys and plain text values that never reach plan or state. Terraform cannot detect changes to them, so bump `data_wo_version` to apply new values. Needs Terraform 1.11 or later.",
				ElementType:         types.StringType,
				Optional:            true,
				WriteOnly:           true,
				Validators:          []validator.Map{mapvalidator.AlsoRequires(path.MatchRoot("data_wo_version"))},
			},
			"data_wo_version": schema.Int64Attribute{
				MarkdownDescription: "Change this to apply the current `data_wo` values.",
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AlsoRequires(path.MatchRoot("data_wo"))},
			},
			"data_hash": schema.StringAttribute{
				MarkdownDescription: "Changes when the data changes. Use it as a pod template annotation to restart workloads when the secret changes. With `data_wo` it only reflects `data_wo_version`, so no value derived from the secret reaches state.",
				Computed:            true,
			},
		},
	}
}

func (r *secretResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return append(connectionValidators(), resourcevalidator.ExactlyOneOf(path.MatchRoot("data"), path.MatchRoot("data_wo")))
}

// ModifyPlan sets data_hash during plan where possible, so workloads using it
// as an annotation show their restart in the same plan as the secret change.
func (r *secretResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hash, diags := planDataHash(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("data_hash"), hash)...)
}

func planDataHash(ctx context.Context, m secretModel) (types.String, diag.Diagnostics) {
	if m.Data.IsUnknown() || m.DataWOVersion.IsUnknown() {
		return types.StringUnknown(), nil
	}
	if !m.DataWOVersion.IsNull() {
		return types.StringValue(hashString("version:" + strconv.FormatInt(m.DataWOVersion.ValueInt64(), 10))), nil
	}
	if m.Data.IsNull() {
		return types.StringNull(), nil
	}
	for _, value := range m.Data.Elements() {
		if value.IsUnknown() {
			return types.StringUnknown(), nil
		}
	}
	data, diags := stringMap(ctx, m.Data)
	if diags.HasError() {
		return types.StringUnknown(), diags
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		diags.AddError("Failed to hash the secret data", err.Error())
		return types.StringUnknown(), diags
	}
	return types.StringValue(hashString("data:" + string(encoded))), diags
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func buildSecret(ctx context.Context, m secretModel, data map[string]string) (*unstructured.Unstructured, error) {
	obj := newObject("v1", "Secret", m.Namespace.ValueString(), m.Name.ValueString())
	if diags := setMetadata(ctx, obj, m.Labels, m.Annotations); diags.HasError() {
		return nil, diagsError(diags)
	}
	if err := unstructured.SetNestedField(obj.Object, m.Type.ValueString(), "type"); err != nil {
		return nil, err
	}
	if len(data) > 0 {
		encoded := make(map[string]any, len(data))
		for k, v := range data {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		if err := unstructured.SetNestedMap(obj.Object, encoded, "data"); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// desiredSecretData returns the values to apply. Write-only values exist only in
// the configuration, never in the plan.
func desiredSecretData(ctx context.Context, plan, config secretModel) (map[string]string, diag.Diagnostics) {
	if !plan.Data.IsNull() {
		return stringMap(ctx, plan.Data)
	}
	return stringMap(ctx, config.DataWO)
}

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	namespace, name := plan.Namespace.ValueString(), plan.Name.ValueString()
	exists, err := client.Exists(ctx, secretRef(namespace, name))
	if err != nil {
		resp.Diagnostics.AddError("Failed to check the secret", err.Error())
		return
	}
	if exists {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Secret already exists",
			"Secret "+namespace+"/"+name+" already exists: import it so that it has one owner.")
		return
	}

	resp.Diagnostics.Append(applySecret(ctx, client, &plan, config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(objectID(plan.Cluster, namespace, name))
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func applySecret(ctx context.Context, client *kube.Client, plan *secretModel, config secretModel) diag.Diagnostics {
	data, diags := desiredSecretData(ctx, *plan, config)
	if diags.HasError() {
		return diags
	}
	obj, err := buildSecret(ctx, *plan, data)
	if err != nil {
		diags.AddError("Failed to build the secret", err.Error())
		return diags
	}
	if _, err := client.Apply(ctx, obj); err != nil {
		diags.AddError("Failed to apply the secret", err.Error())
		return diags
	}
	plan.DataWO = types.MapNull(types.StringType)
	return diags
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, data.Cluster, data.Kubeconfig)
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}

	obj, err := client.Get(ctx, secretRef(data.Namespace.ValueString(), data.Name.ValueString()))
	if isGone(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read the secret", err.Error())
		return
	}

	live, err := decodeSecretData(obj)
	if err != nil {
		resp.Diagnostics.AddError("Failed to decode the secret", err.Error())
		return
	}

	labels, diags := trackedMap(ctx, data.Labels, obj.GetLabels())
	resp.Diagnostics.Append(diags...)
	annotations, diags := trackedMap(ctx, data.Annotations, obj.GetAnnotations())
	resp.Diagnostics.Append(diags...)
	values, diags := trackedMap(ctx, data.Data, live)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.Labels = labels
	data.Annotations = annotations
	data.Data = values
	if liveType, ok, _ := unstructured.NestedString(obj.Object, "type"); ok {
		data.Type = types.StringValue(liveType)
	}

	hash, diags := planDataHash(ctx, data)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.DataHash = hash
	resp.Diagnostics.Append(resp.State.Set(ctx, data)...)
}

func decodeSecretData(obj *unstructured.Unstructured) (map[string]string, error) {
	encoded, _, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil {
		return nil, err
	}
	decoded := make(map[string]string, len(encoded))
	for key, value := range encoded {
		bytes, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode key %q: %w", key, err)
		}
		decoded[key] = string(bytes)
	}
	return decoded, nil
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config secretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, plan.Cluster, plan.Kubeconfig)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}
	resp.Diagnostics.Append(applySecret(ctx, client, &plan, config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := connect(ctx, state.Cluster, state.Kubeconfig)
	if isGone(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the cluster", err.Error())
		return
	}
	if err := client.Delete(ctx, secretRef(state.Namespace.ValueString(), state.Name.ValueString()), deleteTimeout); err != nil {
		resp.Diagnostics.AddError("Failed to delete the secret", err.Error())
	}
}

func (r *secretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importNamespaced(ctx, req, resp)
}
