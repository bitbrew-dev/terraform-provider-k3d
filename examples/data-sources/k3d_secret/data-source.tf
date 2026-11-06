data "k3d_secret" "api" {
  cluster   = "demo"
  namespace = "app"
  name      = "api"
}
