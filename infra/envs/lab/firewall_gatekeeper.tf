# Gatekeeper's admission webhook (deploy/gatekeeper, M6). Same pattern as the Istio rule in
# network.tf: GKE opens only 443 and 10250 from the control plane to private nodes, and the API
# server calls webhook pods directly on their container port, not through the Service. Without
# this rule every call times out after 3 s; the webhook fails open, so requests still pass, but
# every create and update in the cluster waits for the timeout and nothing is checked at admission.
# The port is controllerManager.port in deploy/gatekeeper/values.yaml; change both together.
resource "google_compute_firewall" "gatekeeper_webhook" {
  name        = "mamori-allow-control-plane-gatekeeper"
  network     = google_compute_network.vpc.id
  description = "GKE control plane to the Gatekeeper admission webhook on the nodes (8443)."
  direction   = "INGRESS"
  priority    = 1000

  source_ranges = [local.control_plane_cidr]
  target_tags   = [local.node_tag]

  allow {
    protocol = "tcp"
    ports    = ["8443"]
  }
}
