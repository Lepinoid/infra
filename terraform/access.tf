resource "cloudflare_zero_trust_access_application" "grafana" {
  account_id   = var.cloudflare_account_id
  name         = "Infra"
  domain       = "grafana.lepinoid.net"
  type         = "self_hosted"
  allowed_idps = []

  policies = [{
    name       = "Infra"
    decision   = "allow"
    precedence = 1

    include = [{
      group = {
        id = "54107b50-dadf-4d67-8ab3-ea3b794b2b9d"
      }
    }]
  }]
}
