data "k3d_config_map" "settings" {
  cluster   = "demo"
  namespace = "app"
  name      = "settings"
}
