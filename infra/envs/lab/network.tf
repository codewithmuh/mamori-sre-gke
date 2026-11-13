resource "google_compute_network" "vpc" {
  name                    = "mamori-vpc"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}

# Sizes are generous for a 3-node cluster because ranges are hard to change later:
# nodes /22, pods /17 (each node takes a /25 for its 64 pods, so up to 256 nodes), Services /22.
resource "google_compute_subnetwork" "tokyo" {
  name          = "mamori-tokyo"
  region        = local.region
  network       = google_compute_network.vpc.id
  ip_cidr_range = "10.10.0.0/22"

  # Private nodes reach Google APIs (Artifact Registry, logging, monitoring) without NAT.
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = "10.20.0.0/17"
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = "10.30.0.0/22"
  }

  # VPC flow logs stay off: they are billed per GiB and nothing needs them yet. Turn them on when a
  # game day needs network evidence.
}

resource "google_compute_router" "router" {
  name    = "mamori-router"
  region  = local.region
  network = google_compute_network.vpc.id
}

# Private nodes have no external IP. NAT lets them pull images from registries outside Google
# (Argo CD and Istio images come from quay.io, ghcr.io and docker.io).
resource "google_compute_router_nat" "nat" {
  name   = "mamori-nat"
  router = google_compute_router.router.name
  region = local.region

  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  subnetwork {
    name                    = google_compute_subnetwork.tokyo.id
    source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
  }

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# GKE opens only 443 and 10250 from the control plane to private nodes. Admission webhooks served
# by pods on other ports need their own rule, or every request they watch times out.
# Istio's sidecar injector and config validation webhook listen on 15017 (Istio's GKE setup
# guide). Source: the control plane's range, which GKE reports as masterIpv4CidrBlock and which
# gke.tf sets to local.control_plane_cidr. Gatekeeper (M6) will need 8443 added here.
resource "google_compute_firewall" "control_plane_webhooks" {
  name        = "mamori-allow-control-plane-webhooks"
  network     = google_compute_network.vpc.id
  description = "GKE control plane to admission webhooks on the nodes (Istio 15017)."
  direction   = "INGRESS"
  priority    = 1000

  source_ranges = [local.control_plane_cidr]
  target_tags   = [local.node_tag]

  allow {
    protocol = "tcp"
    ports    = ["15017"]
  }
}

# Google's load balancer proxies and health checkers reach the Istio ingress gateway pods (traffic
# on 80, readiness on 15021) from these two Google-owned ranges. The GKE Gateway controller
# should create an equivalent rule itself; this one makes the path explicit and doesn't depend on
# that.
resource "google_compute_firewall" "lb_to_ingress_gateway" {
  name        = "mamori-allow-lb-to-ingress-gateway"
  network     = google_compute_network.vpc.id
  description = "Google load balancer and health checks to the Istio ingress gateway pods."
  direction   = "INGRESS"
  priority    = 1000

  source_ranges = ["35.191.0.0/16", "130.211.0.0/22"]
  target_tags   = [local.node_tag]

  allow {
    protocol = "tcp"
    ports    = ["80", "15021"]
  }
}
