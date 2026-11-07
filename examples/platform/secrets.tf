locals {
  enable_world   = var.world_path != ""
  enable_taproom = var.taproom_chart_path != ""
  enable_aquor   = var.aquor_chart_path != ""
  enable_areo    = var.areo_chart_path != ""

  namespace_names = merge(
    { for name, namespace in k3d_namespace.plain : name => namespace.name },
    { for name, namespace in k3d_namespace.injected : name => namespace.name },
  )

  pull_secret_type = "kubernetes.io/dockerconfigjson"

  pull_secret = { type = local.pull_secret_type, labels = null }
  opaque      = { type = "Opaque", labels = null }

  secret_catalog = {
    "cloudflare/cloudflare-bitbrew-secret" = merge(local.opaque, { enabled = local.enable_world })
    "cloudflare/ghcr-secret"               = merge(local.pull_secret, { enabled = local.enable_world })
    "keycloak/keycloak-db"                 = merge(local.opaque, { enabled = local.enable_world })
    "keycloak/keycloak-bootstrap-admin"    = merge(local.opaque, { enabled = local.enable_world })
    "worker/ntfy-auth"                     = merge(local.opaque, { enabled = local.enable_world })
    "arc-runners/github-arc-app"           = merge(local.opaque, { enabled = local.enable_world })
    "app/ghcr-token"                       = merge(local.pull_secret, { enabled = local.enable_taproom || local.enable_aquor })
    "app/aquor-database"                   = merge(local.opaque, { enabled = local.enable_aquor })
    "app/aquor-redis"                      = merge(local.opaque, { enabled = local.enable_aquor })
    "app/aquor-api"                        = merge(local.opaque, { enabled = local.enable_aquor })
    "app/aquor-worker"                     = merge(local.opaque, { enabled = local.enable_aquor })
    "app/aquor-providers"                  = merge(local.opaque, { enabled = local.enable_aquor })
    "airflow/areo-secrets"                 = merge(local.opaque, { enabled = local.enable_areo })
    "airflow/areo-worker-secrets"          = merge(local.opaque, { enabled = local.enable_areo })
    "airflow/aws-creds-secret"             = merge(local.opaque, { enabled = local.enable_areo })
    "airflow/git-ssh-key"                  = merge(local.opaque, { enabled = local.enable_areo })
    "airflow/ghcr-secret"                  = merge(local.pull_secret, { enabled = local.enable_areo })
    "airflow/dkcr-secret"                  = merge(local.pull_secret, { enabled = local.enable_areo })
    "argocd/areo-repo-secret" = {
      enabled = local.enable_areo
      type    = "Opaque"
      labels  = { "argocd.argoproj.io/secret-type" = "repository" }
    }
  }

  secrets = { for key, secret in local.secret_catalog : key => secret if secret.enabled }
}

resource "k3d_secret" "platform" {
  for_each = local.secrets

  cluster         = k3d_cluster.platform.id
  namespace       = local.namespace_names[split("/", each.key)[0]]
  name            = split("/", each.key)[1]
  type            = each.value.type
  labels          = each.value.labels
  data_wo         = var.secret_values[each.key]
  data_wo_version = var.secret_version
}
