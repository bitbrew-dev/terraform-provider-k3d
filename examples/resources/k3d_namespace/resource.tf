resource "k3d_namespace" "app" {
  cluster = k3d_cluster.main.id
  name    = "app"

  labels = {
    "istio-injection" = "enabled"
  }
}
