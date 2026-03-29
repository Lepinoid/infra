resource "cloudflare_zero_trust_tunnel_cloudflared" "lepinoid" {
  account_id = var.cloudflare_account_id
  name       = "lepinoid-k8s"
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "lepinoid" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lepinoid.id

  config = {
    ingress = [
      {
        hostname = "grafana.lepinoid.net"
        service  = "http://grafana.lepinoid.svc.cluster.local:80"
        origin_request = {
          access = {
            required  = true
            team_name = "lepinoid"
            aud_tag   = [cloudflare_zero_trust_access_application.grafana.aud]
          }
        }
      },
      {
        hostname = "livesync.lepinoid.net"
        service  = "http://couchdb.lepinoid.svc.cluster.local:5984"
      },
      {
        service = "http_status:404"
      }
    ]
  }
}

resource "cloudflare_dns_record" "grafana" {
  zone_id = var.cloudflare_zone_id
  name    = "grafana"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.lepinoid.id}.cfargotunnel.com"
  type    = "CNAME"
  proxied = true
  ttl     = 1
}

resource "cloudflare_dns_record" "livesync" {
  zone_id = var.cloudflare_zone_id
  name    = "livesync"
  content = "${cloudflare_zero_trust_tunnel_cloudflared.lepinoid.id}.cfargotunnel.com"
  type    = "CNAME"
  proxied = true
  ttl     = 1
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "lepinoid" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.lepinoid.id
}

output "tunnel_token" {
  value     = data.cloudflare_zero_trust_tunnel_cloudflared_token.lepinoid.token
  sensitive = true
}
