# The public way in: the GKE Gateway (deploy/edge) creates the global external Application Load
# Balancer and refers to the three resources below by name:
#   - address mamori-frontend-ip    (Gateway spec.addresses, type NamedAddress)
#   - certificate map mamori-cert-map (annotation networking.gke.io/certmap)
#   - security policy mamori-frontend-armor (GCPBackendPolicy securityPolicy)
# They live in Terraform so the IP and certificate exist before the Gateway does.

resource "google_compute_global_address" "frontend" {
  name         = "mamori-frontend-ip"
  address_type = "EXTERNAL"
  ip_version   = "IPV4"
}

# Per-client-IP rate limit with a 429, everything else allowed.
#
# Threshold (estimates, not measured): one Online Boutique page view is about 20 requests (HTML,
# CSS, JS, product images). A fast human clicking a page every 3 s makes about 400 requests a
# minute with a cold cache, far fewer once the browser caches static files. 600 a minute
# (10 a second sustained) leaves room for that and for a few people behind one NAT, while a
# scraper or flood from one address gets 429s. Probes and synthetics send a few requests a minute.
# The in-cluster load generator doesn't pass through the load balancer at all. Raise it with
# armor_rate_limit_per_minute for a capacity test from one machine.
resource "google_compute_security_policy" "frontend" {
  name        = "mamori-frontend-armor"
  description = "Per-client-IP rate limit for the public frontend."
  type        = "CLOUD_ARMOR"

  rule {
    action      = "throttle"
    priority    = 1000
    description = "Rate limit per client IP"

    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }

    rate_limit_options {
      conform_action = "allow"
      exceed_action  = "deny(429)"
      enforce_on_key = "IP"

      rate_limit_threshold {
        count        = var.armor_rate_limit_per_minute
        interval_sec = 60
      }
    }
  }

  rule {
    action      = "allow"
    priority    = 2147483647
    description = "Default rule: allow"

    match {
      versioned_expr = "SRC_IPS_V1"
      config {
        src_ip_ranges = ["*"]
      }
    }
  }
}

# Google-managed certificate validated by DNS, so it can be issued before the A record points to
# the load balancer. The CNAME to add is in the dns_records_to_add output.
resource "google_certificate_manager_dns_authorization" "frontend" {
  name        = "mamori-frontend-dnsauth"
  location    = "global"
  description = "DNS authorization for ${var.frontend_hostname}"
  domain      = var.frontend_hostname
  type        = "FIXED_RECORD"
}

resource "google_certificate_manager_certificate" "frontend" {
  name        = "mamori-frontend-cert"
  location    = "global"
  description = "Google-managed certificate for ${var.frontend_hostname}"
  scope       = "DEFAULT"

  managed {
    domains            = [var.frontend_hostname]
    dns_authorizations = [google_certificate_manager_dns_authorization.frontend.id]
  }
}

resource "google_certificate_manager_certificate_map" "frontend" {
  name        = "mamori-cert-map"
  description = "Certificate map of the mamori frontend Gateway"
}

resource "google_certificate_manager_certificate_map_entry" "frontend" {
  name         = "mamori-frontend"
  description  = "Serves ${var.frontend_hostname}"
  map          = google_certificate_manager_certificate_map.frontend.name
  hostname     = var.frontend_hostname
  certificates = [google_certificate_manager_certificate.frontend.id]
}
