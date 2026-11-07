locals {
  keycloak_files    = local.enable_world ? ["${var.world_path}/keycloak/bitbrew/app/manifests.yml"] : []
  cloudflared_files = local.enable_world ? ["${var.world_path}/cloudflare/bitbrew/ops/value.yml"] : []

  keycloak_documents = flatten([
    for path in local.keycloak_files : [
      for document in split("\n---\n", file(path)) : yamldecode(document)
      if can(yamldecode(document).kind)
    ]
  ])

  keycloak_objects = {
    for object in local.keycloak_documents : "${object.kind}/${object.metadata.name}" => object
    if object.kind != "Namespace"
  }

  ntfy_files = local.enable_world ? ["service_account", "pvc", "deployment", "service", "network"] : []

  ntfy_objects = {
    for file_name in local.ntfy_files : file_name => yamldecode(file("${var.world_path}/ntfy/${file_name}.yml"))
  }

  world_objects = merge(
    { for key, object in local.keycloak_objects : "keycloak/${key}" => object },
    { for key, object in local.ntfy_objects : "ntfy/${key}" => merge(object, { metadata = merge(object.metadata, { namespace = local.namespace_names["worker"] }) }) },
    { for path in local.cloudflared_files : "cloudflared/deployment" => yamldecode(file(path)) },
  )

  arc_charts = "oci://ghcr.io/actions/actions-runner-controller-charts"
  arc_ops    = "${var.world_path}/k3d/arc/ops"
}

resource "k3d_manifest" "world" {
  for_each = local.world_objects

  cluster  = k3d_cluster.platform.id
  manifest = yamlencode(each.value)

  depends_on = [k3d_secret.platform, k3d_namespace.plain, k3d_namespace.injected]
}

resource "k3d_helm_release" "arc_controller" {
  count = local.enable_world ? 1 : 0

  cluster       = k3d_cluster.platform.id
  namespace     = k3d_namespace.plain["arc-systems"].name
  name          = "arc"
  chart         = "${local.arc_charts}/gha-runner-scale-set-controller"
  chart_version = "0.14.2"
  values        = [file("${local.arc_ops}/values-controller.yml")]
}

resource "k3d_helm_release" "arc_runners" {
  count = local.enable_world ? 1 : 0

  cluster       = k3d_cluster.platform.id
  namespace     = k3d_namespace.plain["arc-runners"].name
  name          = "k3d-arm64"
  chart         = "${local.arc_charts}/gha-runner-scale-set"
  chart_version = "0.14.2"
  values        = [file("${local.arc_ops}/values-runner.yml")]

  depends_on = [k3d_helm_release.arc_controller, k3d_secret.platform]
}
