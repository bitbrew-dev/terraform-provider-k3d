# Terraform Provider for k3d

Build a whole [k3d](https://k3d.io) platform from one configuration: the cluster, its registries, Istio or Traefik, namespaces, secrets, Helm releases, and Argo CD applications. From an empty Docker host, one `terraform apply` creates all of it.

## Why this provider

A cluster and everything inside it can share one apply. Providers that talk to the Kubernetes API during plan need the cluster to exist first, which usually means two applies.

| Rule | How it works |
|---|---|
| No API calls while planning something new | Objects connect at apply, so a cluster, a CRD, and a custom resource can all be created together |
| Secrets can stay out of state | `data_wo` and `values_wo` never reach plan or state. Bump a version number to apply new values |
| Everything is pinned | The k3s image, chart versions, and Istio version are required, and moving tags are rejected |

## Resources and data sources

| Area | Resources | Data sources |
|---|---|---|
| Cluster | `k3d_cluster`, `k3d_registry` | `k3d_cluster`, `k3d_registry`, `k3d_context` |
| Kubernetes | `k3d_namespace`, `k3d_config_map`, `k3d_secret`, `k3d_job`, `k3d_manifest` | `k3d_namespace`, `k3d_config_map`, `k3d_secret` |
| Platform | `k3d_helm_release`, `k3d_istio`, `k3d_argocd_application` | |

Full reference: [docs/index.md](docs/index.md).

## Examples

- [examples/minimal](examples/minimal): a small cluster and an app, using public images only. Tested on every acceptance run.
- [examples/coldbrew](examples/coldbrew): a full platform with Istio, Argo CD, charts from other repositories, and write-only secrets.

## Using it before a release

The provider is not on the Terraform Registry yet. Build it and point Terraform at it:

```bash
make build
```

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "bitbrew-dev/k3d" = "/path/to/terraform-provider-k3d/dist"
  }
  direct {}
}
```

With `dev_overrides`, skip `terraform init` and run `terraform plan` directly. Terraform 1.11 or later is required.

## Good to know

- **State holds credentials:** `k3d_cluster.kubeconfig` includes an admin client key, so use an encrypted state backend.
- **Replacing a cluster:** objects recreate with it only if they use `cluster = k3d_cluster.<name>.id`, not a literal name.
- **Remote Docker:** `docker_host` accepts `tcp://` and `ssh://`, but only local sockets are tested so far. Set `api.host` and `api.port` for a remote host.
- **Removing a cluster keeps host data:** `volumes` sources live on the Docker host and are not deleted.

## Development

| Command | What it does |
|---|---|
| `make test` | Unit tests with the race detector |
| `make testacc` | Acceptance tests against real clusters on local Docker. Takes about 30 minutes and needs internet access |
| `make validate-examples` | Validates the examples against a build of this checkout |
| `make docs` | Regenerates [docs](docs) from the schemas and `examples/` |
