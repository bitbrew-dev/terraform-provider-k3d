variable "cluster_name" {
  description = "Name of the k3d cluster."
  type        = string
  default     = "platform"
}

variable "servers" {
  description = "Server nodes. Use an odd number, because an even number of etcd members adds no fault tolerance."
  type        = number
  default     = 3
}

variable "agents" {
  description = "Agent nodes."
  type        = number
  default     = 2
}

variable "k3s_image" {
  description = "k3s image, pinned."
  type        = string
  default     = "rancher/k3s:v1.32.4-k3s1"
}

variable "storage_path" {
  description = "Existing folder on the Docker host for local-path volumes. It survives destroying the cluster."
  type        = string
}

variable "api_port" {
  description = "Host port for the Kubernetes API."
  type        = number
  default     = 6443
}

variable "api_host" {
  description = "Host name for the kubeconfig. Set it when Docker is on a remote host."
  type        = string
  default     = null
}

variable "tls_sans" {
  description = "Extra names and IPs for the API certificate, such as a Tailscale address."
  type        = list(string)
  default     = []
}

variable "extra_k3s_args" {
  description = "Extra arguments for k3s servers, such as --egress-selector-mode=disabled."
  type        = list(string)
  default     = []
}

variable "registries_config" {
  description = "Contents of k3s registries.yaml. Stored in Terraform state."
  type        = string
  default     = null
  sensitive   = true
}

variable "http_port" {
  description = "Host port for HTTP through the gateway."
  type        = number
  default     = 80
}

variable "https_port" {
  description = "Host port for HTTPS through the gateway."
  type        = number
  default     = 443
}

variable "argocd_port" {
  description = "Host port for the Argo CD UI."
  type        = number
  default     = 8081
}

variable "istio_version" {
  description = "Istio version."
  type        = string
  default     = "1.30.1"
}

variable "argocd_chart_version" {
  description = "Version of the argo-cd chart. 7.7.5 is Argo CD v2.13.1."
  type        = string
  default     = "7.7.5"
}

variable "guestbook_revision" {
  description = "Git commit of argocd-example-apps to deploy. Pin it, because HEAD is not reproducible."
  type        = string
  default     = "8088f4c0d970abb09e250248cc97e35623447cb5"
}

variable "enable_secrets" {
  description = "Create the acme-web-config Secret from secret_values and load it into acme-web."
  type        = bool
  default     = false
}

variable "secret_values" {
  description = "Secret contents keyed by namespace/name, then by key. Never stored in state. Set with TF_VAR_secret_values."
  type        = map(map(string))
  default     = {}
  sensitive   = true
}

variable "secret_version" {
  description = "Bump this to push the current secret_values into the cluster."
  type        = number
  default     = 1
}
