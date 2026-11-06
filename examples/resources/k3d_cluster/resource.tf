resource "k3d_cluster" "main" {
  name    = "demo"
  image   = "rancher/k3s:v1.32.4-k3s1"
  servers = 3
  agents  = 2
  traefik = false

  api = {
    port = 6445
  }

  ports = [
    { host_port = 8080, container_port = 30080 },
  ]

  volumes = [
    { source = "/data/k3d", destination = "/var/lib/rancher/k3s/storage" },
  ]

  k3s_args = [
    { arg = "--tls-san=k3d.example.com", node_filters = ["server:*"] },
  ]

  node_labels = [
    { key = "tier", value = "worker", node_filters = ["agent:*"] },
  ]

  registries = {
    use = [k3d_registry.cache.ref]
  }
}
