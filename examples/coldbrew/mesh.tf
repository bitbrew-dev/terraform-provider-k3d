locals {
  plain_namespaces = {
    "argocd"      = { "istio-injection" = "disabled", name = "argocd" }
    "keycloak"    = { "istio-injection" = "disabled" }
    "cloudflare"  = {}
    "arc-systems" = { "istio-injection" = "disabled", purpose = "ci", type = "control-plane" }
    "arc-runners" = { "istio-injection" = "disabled", purpose = "ci", type = "worker-plane" }
  }

  injected_namespaces = {
    "app"     = { purpose = "applications", type = "control-plane" }
    "worker"  = { purpose = "worker", type = "worker-plane" }
    "airflow" = { purpose = "workflow-automation", type = "worker-plane" }
    "monitor" = { purpose = "monitoring", name = "monitor" }
  }

  protected_namespaces = toset(["app", "worker"])
}

resource "k3d_namespace" "istio_system" {
  cluster = k3d_cluster.coldbrew.id
  name    = "istio-system"
}

resource "k3d_istio" "mesh" {
  cluster   = k3d_cluster.coldbrew.id
  namespace = k3d_namespace.istio_system.name
  version   = var.istio_version

  istiod_values = [yamlencode({
    meshConfig = { accessLogFile = "/dev/stdout", enableTracing = false }
    pilot = {
      resources = {
        requests = { cpu = "300m", memory = "512Mi" }
        limits   = { cpu = "500m", memory = "1Gi" }
      }
    }
  })]

  gateway = {
    http_node_port  = local.gateway_http_node_port
    https_node_port = local.gateway_https_node_port
  }
}

resource "k3d_namespace" "plain" {
  for_each = local.plain_namespaces

  cluster = k3d_cluster.coldbrew.id
  name    = each.key
  labels  = each.value
}

resource "k3d_namespace" "injected" {
  for_each = local.injected_namespaces

  cluster = k3d_cluster.coldbrew.id
  name    = each.key
  labels  = merge(each.value, { "istio-injection" = "enabled" })

  depends_on = [k3d_istio.mesh]
}

resource "k3d_manifest" "default_gateway" {
  cluster = k3d_cluster.coldbrew.id

  manifest = yamlencode({
    apiVersion = "networking.istio.io/v1"
    kind       = "Gateway"
    metadata   = { name = "default-gateway", namespace = k3d_namespace.istio_system.name }
    spec = {
      selector = { istio = "ingressgateway" }
      servers = [
        { port = { number = 80, name = "http", protocol = "HTTP" }, hosts = ["*"] },
        { port = { number = 443, name = "https", protocol = "HTTPS" }, hosts = ["*"], tls = { mode = "PASSTHROUGH" } },
      ]
    }
  })

  depends_on = [k3d_istio.mesh]
}

resource "k3d_manifest" "default_deny_ingress" {
  for_each = local.protected_namespaces

  cluster = k3d_cluster.coldbrew.id

  manifest = yamlencode({
    apiVersion = "networking.k8s.io/v1"
    kind       = "NetworkPolicy"
    metadata   = { name = "default-deny-ingress", namespace = k3d_namespace.injected[each.key].name }
    spec       = { podSelector = {}, policyTypes = ["Ingress"] }
  })
}

resource "k3d_manifest" "allow_same_namespace" {
  for_each = local.protected_namespaces

  cluster = k3d_cluster.coldbrew.id

  manifest = yamlencode({
    apiVersion = "networking.k8s.io/v1"
    kind       = "NetworkPolicy"
    metadata   = { name = "allow-same-namespace", namespace = k3d_namespace.injected[each.key].name }
    spec = {
      podSelector = {}
      policyTypes = ["Ingress"]
      ingress     = [{ from = [{ podSelector = {} }] }]
    }
  })
}

resource "k3d_manifest" "allow_monitoring" {
  for_each = local.protected_namespaces

  cluster = k3d_cluster.coldbrew.id

  manifest = yamlencode({
    apiVersion = "networking.k8s.io/v1"
    kind       = "NetworkPolicy"
    metadata   = { name = "allow-monitoring", namespace = k3d_namespace.injected[each.key].name }
    spec = {
      podSelector = {}
      policyTypes = ["Ingress"]
      ingress     = [{ from = [{ namespaceSelector = { matchLabels = { purpose = "monitoring" } } }] }]
    }
  })
}
