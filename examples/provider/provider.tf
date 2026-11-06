terraform {
  required_version = ">= 1.11"

  required_providers {
    k3d = {
      source = "bitbrew-dev/k3d"
    }
  }
}

provider "k3d" {
  docker_host = "unix:///var/run/docker.sock"
}
