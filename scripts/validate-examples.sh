#!/usr/bin/env bash
# Validates every example against the provider built from this checkout. The
# provider is not published, so terraform init cannot find it: this uses
# dev_overrides from a temporary CLI config and leaves ~/.terraformrc alone.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp:?}"' EXIT

go build -C "$root" -o "$tmp/terraform-provider-k3d" .

cat > "$tmp/terraformrc" <<EOF
provider_installation {
  dev_overrides {
    "bitbrew-dev/k3d" = "$tmp"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE="$tmp/terraformrc"

status=0
for example in "$root"/examples/*/; do
  [ -f "$example/versions.tf" ] || continue
  printf 'validating %s\n' "${example#"$root"/}"
  terraform -chdir="$example" validate -no-color | grep -vE 'Provider development overrides|overrides are set|bitbrew-dev/k3d in|behavior may|applying changes|releases\.|^$|Warning:' || status=$?
done

terraform -chdir="$root/examples" fmt -check -recursive
exit "$status"
