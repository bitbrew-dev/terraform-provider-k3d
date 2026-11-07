resource "k3d_helm_release" "acme_web" {
  cluster   = k3d_cluster.platform.id
  namespace = k3d_namespace.injected["app"].name
  name      = "acme-web"
  src_path  = abspath("${path.module}/charts/acme-web")

  values = [yamlencode({
    existingSecret = var.enable_secrets ? "acme-web-config" : ""
  })]

  depends_on = [k3d_manifest.default_gateway, k3d_secret.platform]
}

resource "k3d_manifest" "acme_web_route" {
  cluster = k3d_cluster.platform.id

  manifest = yamlencode({
    apiVersion = "networking.istio.io/v1"
    kind       = "VirtualService"
    metadata   = { name = "acme-web", namespace = k3d_namespace.injected["app"].name }
    spec = {
      hosts    = ["*"]
      gateways = ["${k3d_namespace.istio_system.name}/default-gateway"]
      http     = [{ route = [{ destination = { host = "acme-web", port = { number = 80 } } }] }]
    }
  })

  depends_on = [k3d_helm_release.acme_web]
}

resource "k3d_argocd_application" "guestbook" {
  cluster   = k3d_cluster.platform.id
  namespace = k3d_namespace.plain["argocd"].name
  name      = "guestbook"

  source = {
    repo_url        = "https://github.com/argoproj/argocd-example-apps.git"
    target_revision = var.guestbook_revision
    path            = "guestbook"
  }

  destination = {
    namespace = k3d_namespace.plain["guestbook"].name
  }

  sync_policy = {
    automated = true
    prune     = true
    self_heal = true
  }

  depends_on = [k3d_helm_release.argocd]
}
