locals {
  secret_catalog = {
    "app/acme-web-config" = { enabled = var.enable_secrets, type = "Opaque" }
  }

  secrets = { for key, secret in local.secret_catalog : key => secret if secret.enabled }
}

resource "k3d_secret" "platform" {
  for_each = local.secrets

  cluster         = k3d_cluster.platform.id
  namespace       = k3d_namespace.injected[split("/", each.key)[0]].name
  name            = split("/", each.key)[1]
  type            = each.value.type
  data_wo         = var.secret_values[each.key]
  data_wo_version = var.secret_version
}
