# Platform example

A full platform built with this provider: a 3-server, 5-agent cluster, Istio, Argo CD, and apps from other repositories. Everything specific to this platform lives here, not in the provider.

## Layers

The core needs no secrets. Other layers switch on when you set their path.

| Layer | Turn on with | Contains |
|---|---|---|
| Core | Always | Cluster, namespaces, NetworkPolicies, Istio, `default-gateway`, Argo CD |
| Services | `world_path` | Keycloak, ntfy, Cloudflared, and ARC runners, from the `world` repository |
| taproom | `taproom_chart_path`, `taproom_tag` | The taproom chart |
| aquor | `aquor_chart_path`, `aquor_tag`, `aquor_values` | The aquor chart |
| Airflow | `areo_chart_path`, `dags_revision` | The areo chart and the `airflow-dags` Argo CD application |

## What each layer needs

| Need | Core | All layers |
|---|---|---|
| Docker memory | About 3.3 GB when idle, measured | Not measured. The live cluster uses far more, with one agent alone at 6 GB |
| Secrets | None | See below |
| Registry access | Public images | A `ghcr.io` token for private images |

## Run it

Create the storage folder first, because it must already exist on the Docker host:

```bash
mkdir -p "$PWD/data/storage"
```

```bash
export TF_VAR_storage_path="$PWD/data/storage"
```

The provider is not on the Registry yet, so use `dev_overrides` from the [root README](../../README.md) and skip `terraform init`.

```bash
terraform apply
```

```bash
terraform output -raw kubeconfig > kubeconfig.yaml
```

## Secrets

Every secret comes from one variable and is written with `data_wo`, so its values never reach Terraform plan or state. Enabled layers require their keys, so a missing secret fails the plan.

```bash
export TF_VAR_secret_values="$(op inject -i secrets.tmpl.json)"
```

The variable maps `namespace/name` to the keys of that Secret, for example:

```json
{
  "keycloak/keycloak-db": {
    "KC_POSTGRES_USER": "...",
    "KC_POSTGRES_PASSWORD": "..."
  }
}
```

| Layer | Secrets |
|---|---|
| Services | `cloudflare/cloudflare-bitbrew-secret`, `cloudflare/ghcr-secret`, `keycloak/keycloak-db`, `keycloak/keycloak-bootstrap-admin`, `worker/ntfy-auth`, `arc-runners/github-arc-app` |
| taproom or aquor | `app/ghcr-token` |
| aquor | `app/aquor-database`, `app/aquor-redis`, `app/aquor-api`, `app/aquor-worker`, `app/aquor-providers` |
| Airflow | `airflow/areo-secrets`, `airflow/areo-worker-secrets`, `airflow/aws-creds-secret`, `airflow/git-ssh-key`, `airflow/ghcr-secret`, `airflow/dkcr-secret`, `argocd/areo-repo-secret` |

To push changed values, bump `secret_version`. Terraform cannot detect changes to them on its own.

## Differences from the live cluster

| Area | Live | Here | Why |
|---|---|---|---|
| Servers | 2 | 3 | 2 etcd members cannot lose either one |
| Istio install | `istioctl` | Helm charts | Pinned version, and Terraform can detect drift |
| `istio-system` label | `istio-injection: disabled` | None | The gateway gets its proxy image through injection, so its pod never starts with that label |
| Host ports 80, 443 | Mapped to node ports 80, 443 | Mapped to gateway NodePorts 31035, 30903 | Node ports 80 and 443 only answer with a `LoadBalancer` gateway |
| `taproom` image | `latest` | Pinned tag | Reproducible |

## Chart requirements

| Chart | Setting here | Why |
|---|---|---|
| areo | `wait = false` | `airflow-init` is a `post-install` hook, and Helm waits for resources before running hooks, so waiting could hang |
| areo | `take_ownership = true` | The chart creates a Namespace, but secrets need it first |
| aquor | `aquor_tag`, `aquor_values` | The default image tag is `CHANGE_ME`, and `global.host` is per environment |
| Keycloak manifests | Namespace left out of `for_each` | `k3d_namespace` owns it |
| ntfy | Namespace set on each object | The repository uses a Kustomize transformer, and this provider does not run Kustomize |

## Not included yet

- **Monitoring:** Prometheus and Grafana are generated outside this configuration, with no manifest files to reference. Add them with `k3d_helm_release` or `k3d_manifest`.
- **Manual steps:** the GitHub App, GitHub OAuth, the Keycloak `terraform` client, and the SES production request stay outside Terraform.
- **Other Terraform stacks:** Cloudflare and Keycloak realm config stay in `world`. The realm needs Keycloak running, so it is a second stage.
- **Image builds:** these stay in `docker buildx` and `arctl build`.
- **Data:** volume contents need their own backup and restore.
- **Remote Docker:** set `TF_VAR_api_host`, and set `docker_host` in a `provider` block.
