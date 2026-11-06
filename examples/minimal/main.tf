locals {
  node_port = 30080
}

resource "k3d_cluster" "main" {
  name   = var.cluster_name
  image  = "rancher/k3s:v1.32.4-k3s1"
  agents = 1

  ports = [
    { host_port = var.http_port, container_port = local.node_port },
  ]
}

resource "k3d_namespace" "app" {
  cluster = k3d_cluster.main.id
  name    = "app"

  labels = {
    purpose = "example"
  }
}

resource "k3d_config_map" "greeting" {
  cluster   = k3d_cluster.main.id
  namespace = k3d_namespace.app.name
  name      = "greeting"

  data = {
    "index.html" = "<h1>Served by nginx through k3d</h1>\n"
  }
}

resource "k3d_manifest" "web" {
  cluster = k3d_cluster.main.id
  wait    = true

  manifest = yamlencode({
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata = {
      name      = "web"
      namespace = k3d_namespace.app.name
    }
    spec = {
      selector = { matchLabels = { app = "web" } }
      template = {
        metadata = { labels = { app = "web" } }
        spec = {
          containers = [{
            name         = "nginx"
            image        = "nginx:1.27-alpine"
            ports        = [{ containerPort = 80 }]
            volumeMounts = [{ name = "html", mountPath = "/usr/share/nginx/html" }]
          }]
          volumes = [{ name = "html", configMap = { name = k3d_config_map.greeting.name } }]
        }
      }
    }
  })
}

resource "k3d_manifest" "service" {
  cluster = k3d_cluster.main.id

  manifest = yamlencode({
    apiVersion = "v1"
    kind       = "Service"
    metadata = {
      name      = "web"
      namespace = k3d_namespace.app.name
    }
    spec = {
      type     = "NodePort"
      selector = { app = "web" }
      ports    = [{ port = 80, nodePort = local.node_port }]
    }
  })
}

data "k3d_context" "main" {
  cluster = k3d_cluster.main.id
}

output "context" {
  description = "Kubeconfig context name of the cluster."
  value       = data.k3d_context.main.name
}

output "kubeconfig" {
  description = "Kubeconfig for the cluster. Save it with: terraform output -raw kubeconfig > kubeconfig.yaml"
  value       = k3d_cluster.main.kubeconfig
  sensitive   = true
}

output "url" {
  description = "Where the app is served."
  value       = "http://localhost:${var.http_port}"
}
