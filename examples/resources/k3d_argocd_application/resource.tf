resource "k3d_argocd_application" "guestbook" {
  cluster       = k3d_cluster.main.id
  name          = "guestbook"
  wait_for_sync = true

  source = {
    repo_url        = "https://github.com/argoproj/argocd-example-apps.git"
    target_revision = "8088f4c0d970abb09e250248cc97e35623447cb5"
    path            = "guestbook"
  }

  destination = {
    namespace = "guestbook"
  }

  sync_policy = {
    automated    = true
    prune        = true
    self_heal    = true
    sync_options = ["CreateNamespace=true"]
  }

  depends_on = [k3d_helm_release.argocd]
}
