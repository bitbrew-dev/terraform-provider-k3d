locals {
  gateway_http_node_port  = 31035
  gateway_https_node_port = 30903

  server_args = concat(
    [for san in var.tls_sans : "--tls-san=${san}"],
    var.extra_k3s_args,
  )
}

resource "k3d_cluster" "coldbrew" {
  name    = var.cluster_name
  image   = var.k3s_image
  servers = var.servers
  agents  = var.agents
  traefik = false
  timeout = "20m"

  api = {
    host = var.api_host
    port = var.api_port
  }

  ports = concat(
    [
      { host_ip = null, host_port = var.http_port, container_port = local.gateway_http_node_port, protocol = null },
      { host_ip = null, host_port = var.https_port, container_port = local.gateway_https_node_port, protocol = null },
    ],
    [for host_port, node_port in var.admin_ports : { host_ip = null, host_port = tonumber(host_port), container_port = node_port, protocol = null }],
  )

  volumes = [
    { source = var.storage_path, destination = "/var/lib/rancher/k3s/storage", node_filters = null },
  ]

  k3s_args = [for arg in local.server_args : { arg = arg, node_filters = ["server:*"] }]

  registries = var.registries_config == null ? null : {
    use    = null
    config = var.registries_config
  }
}

output "kubeconfig" {
  description = "Kubeconfig for the cluster. Save it with: terraform output -raw kubeconfig > kubeconfig.yaml"
  value       = k3d_cluster.coldbrew.kubeconfig
  sensitive   = true
}

output "api" {
  description = "Kubernetes API URL."
  value       = k3d_cluster.coldbrew.host
}
