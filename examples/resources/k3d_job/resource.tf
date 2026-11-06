resource "k3d_job" "seed" {
  cluster   = k3d_cluster.main.id
  namespace = k3d_namespace.app.name
  name      = "seed"
  image     = "busybox:1.36"
  command   = ["sh", "-c", "echo seeded"]

  pod_annotations = {
    "sidecar.istio.io/inject" = "false"
  }
}
