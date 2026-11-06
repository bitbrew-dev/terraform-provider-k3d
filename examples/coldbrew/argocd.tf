resource "k3d_helm_release" "argocd" {
  cluster       = k3d_cluster.coldbrew.id
  namespace     = k3d_namespace.plain["argocd"].name
  name          = "argocd"
  repository    = "https://argoproj.github.io/argo-helm"
  chart         = "argo-cd"
  chart_version = var.argocd_chart_version
  timeout       = "10m"

  values = [yamlencode({
    configs = {
      params = {
        "server.insecure" = true
      }
    }
  })]
}

resource "k3d_manifest" "argocd_nodeport" {
  cluster = k3d_cluster.coldbrew.id

  manifest = yamlencode({
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "argocd-server-nodeport", namespace = k3d_namespace.plain["argocd"].name }
    spec = {
      type     = "NodePort"
      selector = { "app.kubernetes.io/name" = "argocd-server" }
      ports    = [{ name = "http", port = 80, targetPort = 8080, nodePort = 30092 }]
    }
  })

  depends_on = [k3d_helm_release.argocd]
}
