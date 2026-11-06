resource "k3d_namespace" "mesh" {
  cluster = k3d_cluster.main.id
  name    = "istio-system"
}

resource "k3d_istio" "mesh" {
  cluster   = k3d_cluster.main.id
  namespace = k3d_namespace.mesh.name
  version   = "1.30.1"

  gateway = {
    http_node_port  = 31035
    https_node_port = 30903
  }
}
