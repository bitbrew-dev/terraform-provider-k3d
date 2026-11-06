resource "k3d_registry" "cache" {
  name = "cache"
  port = 5000

  proxy = {
    remote_url = "https://registry-1.docker.io"
  }
}
