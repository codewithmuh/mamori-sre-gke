output "cluster_name" {
  description = "Name of the GKE cluster."
  value       = google_container_cluster.mamori.name
}

output "cluster_location" {
  description = "Zone of the GKE cluster."
  value       = google_container_cluster.mamori.location
}

output "get_credentials_command" {
  description = "Command that writes kubectl credentials for the cluster."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.mamori.name} --zone ${google_container_cluster.mamori.location} --project ${var.project_id}"
}

output "node_pool_name" {
  description = "Name of the node pool that the spend guard scales to zero and `make resume` restores."
  value       = google_container_node_pool.spot.name
}

output "artifact_registry_url" {
  description = "Docker repository URL for the lab's images."
  value       = "${google_artifact_registry_repository.mamori.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.mamori.repository_id}"
}

output "frontend_ip" {
  description = "Global static IP of the frontend load balancer (Gateway NamedAddress mamori-frontend-ip)."
  value       = google_compute_global_address.frontend.address
}

output "dns_records_to_add" {
  description = "Records to create by hand where codewithmuh.com's DNS is hosted. The A record changes after every `make down` / `make up`; check the CNAME too."
  value = {
    frontend = {
      name = "${var.frontend_hostname}."
      type = "A"
      data = google_compute_global_address.frontend.address
    }
    certificate_validation = {
      name = google_certificate_manager_dns_authorization.frontend.dns_resource_record[0].name
      type = google_certificate_manager_dns_authorization.frontend.dns_resource_record[0].type
      data = google_certificate_manager_dns_authorization.frontend.dns_resource_record[0].data
    }
  }
}

output "cert_map_name" {
  description = "Certificate map for the Gateway annotation networking.gke.io/certmap."
  value       = google_certificate_manager_certificate_map.frontend.name
}

output "armor_policy_name" {
  description = "Cloud Armor policy for the GCPBackendPolicy securityPolicy field."
  value       = google_compute_security_policy.frontend.name
}

output "budget_alerts_topic" {
  description = "Pub/Sub topic that receives budget notifications and triggers the spend guard (publish a test message here)."
  value       = google_pubsub_topic.budget_alerts.name
}
