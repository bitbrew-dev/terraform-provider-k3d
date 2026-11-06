resource "k3d_manifest" "gateway" {
  cluster = k3d_cluster.main.id

  manifest = yamlencode({
    apiVersion = "networking.istio.io/v1"
    kind       = "Gateway"
    metadata   = { name = "default", namespace = "istio-system" }
    spec = {
      selector = { istio = "ingressgateway" }
      servers  = [{ port = { number = 80, name = "http", protocol = "HTTP" }, hosts = ["*"] }]
    }
  })

  depends_on = [k3d_istio.mesh]
}

locals {
  documents = [for document in split("\n---\n", file("${path.module}/app.yaml")) : yamldecode(document)]
}

resource "k3d_manifest" "app" {
  for_each = { for object in local.documents : "${object.kind}/${object.metadata.name}" => object }

  cluster  = k3d_cluster.main.id
  manifest = yamlencode(each.value)
}
