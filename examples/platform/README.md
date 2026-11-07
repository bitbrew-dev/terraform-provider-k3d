# Platform example

A full platform built with this provider: a highly available cluster, Istio, Argo CD, an app from a local chart, and an app deployed by Argo CD. It uses public images and needs no accounts or secrets to run.

| Part | What it shows |
|---|---|
| Cluster | 3 servers, 2 agents, host ports, and a storage volume |
| Istio | Installed with Helm charts, with a gateway and a route to `acme-web` |
| Network policies | Default deny for the `app` namespace, allowing the gateway and traffic inside the namespace |
| `acme-web` | A chart from a local path, using `src_path`. It lives in [charts/acme-web](charts/acme-web) |
| `guestbook` | An Argo CD application from a public repository, pinned to a commit |
| `acme-web-config` | An optional write-only Secret, turned on with `enable_secrets` |

## Run it

Create the storage folder first, because it must already exist on the Docker host:

```bash
mkdir -p "$PWD/data/storage"
```

```bash
export TF_VAR_storage_path="$PWD/data/storage"
```

```bash
terraform init
```

```bash
terraform apply
```

```bash
terraform output -raw kubeconfig > kubeconfig.yaml
```

## Try it

| Address | What answers |
|---|---|
| `http://localhost` | `acme-web`, through the Istio gateway |
| `http://localhost:8081` | The Argo CD UI, which can take about 15 seconds to answer after the apply |

Change the host ports with `http_port`, `https_port`, and `argocd_port`.

## Secrets

Secret values come from one variable and are written with `data_wo`, so they never reach Terraform plan or state. The variable maps `namespace/name` to the keys of that Secret:

```bash
export TF_VAR_enable_secrets=true
```

```bash
export TF_VAR_secret_values='{"app/acme-web-config":{"GREETING":"hello"}}'
```

To push changed values, bump `secret_version`. Terraform cannot detect changes to them on its own.

## Design notes

| Choice | Why |
|---|---|
| 3 servers | An even number of etcd members adds no fault tolerance |
| Istio through Helm charts | The version is pinned, and Terraform can detect drift |
| No `istio-injection: disabled` on `istio-system` | The gateway gets its proxy image through injection, so its pod never starts with that label |
| Host ports mapped to gateway NodePorts | Node ports 80 and 443 only answer with a `LoadBalancer` gateway |

## Your own apps

- **Add a chart:** copy the `k3d_helm_release.acme_web` resource and point `src_path` at your chart.
- **Add a Secret:** add an entry to `secret_catalog` in [secrets.tf](secrets.tf). The catalog is separate from `secret_values` because Terraform cannot use a sensitive value in `for_each`.
- **Private images or repositories:** add a Secret of the matching type, and reference it in your chart's values or Argo CD repository settings.
- **Remote Docker:** set `TF_VAR_api_host`, and set `docker_host` in a `provider` block.
- **Data:** volume contents need their own backup and restore.
