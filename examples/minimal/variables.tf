variable "cluster_name" {
  description = "Name of the k3d cluster."
  type        = string
  default     = "minimal"
}

variable "http_port" {
  description = "Port on the Docker host that serves the app."
  type        = number
  default     = 8080
}
