# Zonal Standard cluster: the GKE free tier covers the fee of one zonal cluster (D2).
#
# Authorized networks are on only when master_authorized_networks is set; by default the endpoint
# is open to any address and protected by IAM and RBAC (lab trade-off, see infra/README.md).
#trivy:ignore:AVD-GCP-0061
resource "google_container_cluster" "mamori" {
  name     = local.cluster_name
  location = var.zone

  # `make down` must be able to delete the cluster.
  deletion_protection = false

  # No default node pool. With no initial_node_count, node_config or node_pool here, GKE
  # 1.36.4-gke.1469000 and later create the cluster without one (REGULAR's default was
  # 1.36.4-gke.2377000 on 2026-11-10), so remove_default_node_pool, now deprecated, isn't needed.
  # The only pool is google_container_node_pool.spot below.

  network         = google_compute_network.vpc.id
  subnetwork      = google_compute_subnetwork.tokyo.id
  networking_mode = "VPC_NATIVE"

  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
    stack_type                    = "IPV4"
  }

  # GKE Dataplane V2 enforces NetworkPolicy without Calico. It can only be chosen at creation, and
  # later milestones add network policies.
  datapath_provider = "ADVANCED_DATAPATH"

  # Private nodes, public control-plane endpoint. On current GKE the control plane reaches the
  # nodes through Private Service Connect; the CIDR is optional there, but setting it gives the
  # control plane a known source range for the webhook firewall rule in network.tf.
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = local.control_plane_cidr
  }

  # Without this block the public endpoint accepts connections from any IP; callers still need
  # Google IAM credentials and Kubernetes RBAC. See infra/README.md for the trade-off.
  dynamic "master_authorized_networks_config" {
    for_each = length(var.master_authorized_networks) > 0 ? [1] : []

    content {
      # Otherwise any Google Cloud public IP (anyone's VM or Cloud Shell) would be allowed too.
      gcp_public_cidrs_access_enabled = false

      dynamic "cidr_blocks" {
        for_each = var.master_authorized_networks

        content {
          cidr_block   = cidr_blocks.value.cidr_block
          display_name = cidr_blocks.value.display_name
        }
      }
    }
  }

  # A second way in: the DNS-based endpoint, which IAM guards and authorized networks don't filter.
  # GitHub-hosted runners (the weekly evidence workflow) and a laptop away from home use it with
  # `get-credentials --dns-endpoint`, so master_authorized_networks can pin the IP endpoint to one
  # address without breaking them. Callers need container.clusters.connect (in
  # roles/container.viewer and up) plus Kubernetes RBAC, as on the IP endpoint.
  control_plane_endpoints_config {
    dns_endpoint_config {
      allow_external_traffic = true
    }
  }

  release_channel {
    channel = "REGULAR"
  }

  # Pods get Google credentials through Kubernetes service accounts, never the node's account.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # The frontend is exposed only through a GKE Gateway (class gke-l7-global-external-managed).
  gateway_api_config {
    channel = "CHANNEL_STANDARD"
  }

  addons_config {
    # The Gateway controller needs the HTTP load balancing add-on.
    http_load_balancing {
      disabled = false
    }
  }

  monitoring_config {
    managed_prometheus {
      enabled = true
    }
  }

  enable_shielded_nodes = true
  enable_legacy_abac    = false

  master_auth {
    client_certificate_config {
      issue_client_certificate = false
    }
  }

  # Auto-upgrades start at 03:00 JST, outside game days and working hours.
  maintenance_policy {
    daily_maintenance_window {
      start_time = "18:00" # UTC
    }
  }

  resource_labels = local.labels
}

# Spot nodes: about 40 % cheaper than on-demand, and preemptions are free real incidents (D2).
#
# Quota: 3 x e2-standard-2 is 6 vCPU. A surge upgrade (max_surge = 1) adds a fourth node, so for a
# few minutes the project needs 8 vCPU in the region. A trial account can't ask for more quota, so
# check the region's CPUS and the global CPUS_ALL_REGIONS quota before the first upgrade.
resource "google_container_node_pool" "spot" {
  name     = local.node_pool_name
  cluster  = google_container_cluster.mamori.id
  location = var.zone

  node_count = local.node_pool_size

  # 64 is plenty for about 30 pods per node, uses a /25 of the pods range per node instead of a
  # /24, and on GKE 1.37.2+ lowers the kubelet memory reservation (it scales with max pods).
  # It can't be changed without recreating the pool.
  max_pods_per_node = 64

  # The spend guard sets the size to 0 outside Terraform. Ignoring that drift means an apply after
  # the guard fired doesn't silently bring the nodes (and the spend) back; `make resume` does that
  # on purpose. A changed node_count in this file is still applied.
  ignore_node_count_changes = true

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  # One extra node during upgrades and never fewer than 3 ready nodes.
  upgrade_settings {
    strategy        = "SURGE"
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type = "e2-standard-2"
    spot         = true
    image_type   = "COS_CONTAINERD"

    # pd-standard costs less than half of the default pd-balanced; 30 GB holds the images.
    boot_disk {
      disk_type = "pd-standard"
      size_gb   = 30
    }

    # Dedicated account with only the node role (infra/envs/lab/iam.tf). With Workload Identity,
    # the broad scope is limited by that account's IAM roles, as Google recommends.
    service_account = local.gke_nodes_sa
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    # Pods see the GKE metadata server, which hides the node's credentials.
    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    kubelet_config {
      insecure_kubelet_readonly_port_enabled = "FALSE"
    }

    # The API sets this anyway; stating it stops Terraform from trying to remove it.
    metadata = {
      disable-legacy-endpoints = "true"
    }

    # Target of the firewall rules in network.tf.
    tags = [local.node_tag]

    resource_labels = local.labels
  }

  # Nodes need their logging and monitoring role before they boot.
  depends_on = [google_project_iam_member.gke_nodes]
}
