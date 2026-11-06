resource "k3d_config_map" "settings" {
  cluster   = k3d_cluster.main.id
  namespace = k3d_namespace.app.name
  name      = "settings"

  data = {
    LOG_LEVEL = "info"
  }
}
