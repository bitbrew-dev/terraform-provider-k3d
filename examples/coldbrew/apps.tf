resource "k3d_helm_release" "taproom" {
  count = local.enable_taproom ? 1 : 0

  cluster   = k3d_cluster.coldbrew.id
  namespace = local.namespace_names["app"]
  name      = "taproom"
  src_path  = abspath(var.taproom_chart_path)

  values = [yamlencode({
    image            = { tag = var.taproom_tag }
    imagePullSecrets = [{ name = "ghcr-token" }]
  })]

  depends_on = [k3d_manifest.default_gateway, k3d_secret.platform]
}

resource "k3d_helm_release" "aquor" {
  count = local.enable_aquor ? 1 : 0

  cluster   = k3d_cluster.coldbrew.id
  namespace = local.namespace_names["app"]
  name      = "aquor"
  src_path  = abspath(var.aquor_chart_path)

  values = concat(
    [yamlencode({
      global = { imagePullSecrets = [{ name = "ghcr-token" }] }
      image = {
        api = { repository = "ghcr.io/bitbrew-dev/aquor/api", tag = var.aquor_tag }
        web = { repository = "ghcr.io/bitbrew-dev/aquor/web", tag = var.aquor_tag }
      }
    })],
    var.aquor_values,
  )

  depends_on = [k3d_manifest.default_gateway, k3d_secret.platform]
}

resource "k3d_helm_release" "areo" {
  count = local.enable_areo ? 1 : 0

  cluster   = k3d_cluster.coldbrew.id
  namespace = local.namespace_names["airflow"]
  name      = "areo"
  src_path  = abspath(var.areo_chart_path)

  take_ownership = true
  wait           = false

  depends_on = [k3d_secret.platform]
}

resource "k3d_argocd_application" "airflow_dags" {
  count = local.enable_areo ? 1 : 0

  cluster   = k3d_cluster.coldbrew.id
  namespace = local.namespace_names["argocd"]
  name      = "airflow-dags"

  source = {
    repo_url          = "git@github.com:benbenbang/areo.git"
    target_revision   = var.dags_revision
    path              = "dags"
    directory_recurse = true
  }

  destination = {
    namespace = local.namespace_names["airflow"]
  }

  sync_policy = {
    automated = true
    prune     = true
    self_heal = true
  }

  depends_on = [k3d_helm_release.argocd, k3d_helm_release.areo, k3d_secret.platform]
}
