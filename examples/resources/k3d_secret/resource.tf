variable "api_token" {
  type      = string
  sensitive = true
}

variable "ghcr_auth" {
  type      = string
  sensitive = true
}

resource "k3d_secret" "api" {
  cluster         = k3d_cluster.main.id
  namespace       = k3d_namespace.app.name
  name            = "api"
  data_wo         = { token = var.api_token }
  data_wo_version = 1
}

resource "k3d_secret" "ghcr" {
  cluster   = k3d_cluster.main.id
  namespace = k3d_namespace.app.name
  name      = "ghcr"
  type      = "kubernetes.io/dockerconfigjson"

  data_wo = {
    ".dockerconfigjson" = jsonencode({
      auths = { "ghcr.io" = { auth = var.ghcr_auth } }
    })
  }
  data_wo_version = 1
}
