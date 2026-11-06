resource "k3d_helm_release" "podinfo" {
  cluster       = k3d_cluster.main.id
  namespace     = k3d_namespace.app.name
  name          = "podinfo"
  repository    = "https://stefanprodan.github.io/podinfo"
  chart         = "podinfo"
  chart_version = "6.16.0"

  values = [yamlencode({ replicaCount = 2 })]
}

resource "k3d_helm_release" "runner" {
  cluster       = k3d_cluster.main.id
  namespace     = "arc-systems"
  name          = "arc"
  chart         = "oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller"
  chart_version = "0.14.2"
}

resource "k3d_helm_release" "local" {
  cluster               = k3d_cluster.main.id
  namespace             = k3d_namespace.app.name
  name                  = "taproom"
  src_path              = abspath("${path.module}/../taproom/ops/charts/taproom")
  expected_chart_digest = var.taproom_chart_digest
  set                   = { "image.tag" = "1.0.1" }
}
