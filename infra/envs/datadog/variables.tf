# Only project_id and datadog_delegate_email lack defaults. `make datadog-plan` sets both.

variable "project_id" {
  description = "Google Cloud project ID of the lab, e.g. mamori-lab-123456. The Datadog service account is created here."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "datadog_site" {
  description = "Datadog site of the account, e.g. ap1.datadoghq.com (Japan). Must match datadog.site in deploy/optional/datadog/values.yaml."
  type        = string
  default     = "ap1.datadoghq.com"

  validation {
    condition = contains([
      "datadoghq.com", "us3.datadoghq.com", "us5.datadoghq.com", "datadoghq.eu",
      "ap1.datadoghq.com", "ap2.datadoghq.com", "uk1.datadoghq.com",
    ], var.datadog_site)
    error_message = "datadog_site must be one of Datadog's commercial sites, e.g. ap1.datadoghq.com."
  }
}

variable "datadog_delegate_email" {
  description = "Datadog's per-organisation service account that impersonates ours (the STS delegate). `make datadog-delegate` prints it."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9-]+@[a-z0-9-]+\\.iam\\.gserviceaccount\\.com$", var.datadog_delegate_email))
    error_message = "datadog_delegate_email must be a service account email (name@project.iam.gserviceaccount.com)."
  }
}

variable "frontend_hostname" {
  description = "Public hostname of the shop that the synthetic tests visit over HTTPS."
  type        = string
  default     = "mamori.codewithmuh.com"
}

variable "notification_handles" {
  description = "Datadog @-handles every notification goes to, e.g. [\"@slack-mamori-alerts\"]. Empty: alerts show in Datadog only. Pages come from Cloud Monitoring, not from here."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for h in var.notification_handles : can(regex("^@[A-Za-z0-9_.@-]+$", h))])
    error_message = "Each handle must start with @ and contain no spaces, e.g. @slack-mamori-alerts."
  }
}

variable "synthetics_locations" {
  description = "Managed locations the synthetic tests run from. aws:ap-northeast-1 is Tokyo, outside Google Cloud."
  type        = list(string)
  default     = ["aws:ap-northeast-1"]

  validation {
    condition     = length(var.synthetics_locations) > 0
    error_message = "At least one location is needed."
  }
}

variable "synthetics_status" {
  description = "live or paused. Pausing keeps the tests and their history but stops runs and alerts."
  type        = string
  default     = "live"

  validation {
    condition     = contains(["live", "paused"], var.synthetics_status)
    error_message = "synthetics_status must be live or paused."
  }
}

variable "synthetics_tick_every" {
  description = "Seconds between runs of each synthetic test."
  type        = number
  default     = 300

  validation {
    condition     = var.synthetics_tick_every >= 60 && var.synthetics_tick_every <= 3600
    error_message = "synthetics_tick_every must be between 60 and 3600 seconds."
  }
}

variable "synthetics_retry_count" {
  description = "Times a failed run is retried before the location counts as failed (0-3). A tuning knob for the window."
  type        = number
  default     = 1

  validation {
    condition     = var.synthetics_retry_count >= 0 && var.synthetics_retry_count <= 3 && floor(var.synthetics_retry_count) == var.synthetics_retry_count
    error_message = "synthetics_retry_count must be a whole number from 0 to 3."
  }
}

variable "synthetics_min_failure_duration" {
  description = "Seconds a test must keep failing before its monitor alerts (0 = on the first failed run). A tuning knob for the window."
  type        = number
  default     = 0

  validation {
    condition     = var.synthetics_min_failure_duration >= 0 && var.synthetics_min_failure_duration <= 7200
    error_message = "synthetics_min_failure_duration must be between 0 and 7200 seconds."
  }
}

variable "synthetics_max_step_ms" {
  description = "A step slower than this (milliseconds, whole request) fails the test. Loose on purpose: the SLOs judge latency, this catches pages a shopper would give up on."
  type        = number
  default     = 2000

  validation {
    condition     = var.synthetics_max_step_ms >= 200 && var.synthetics_max_step_ms <= 30000
    error_message = "synthetics_max_step_ms must be between 200 and 30000."
  }
}

variable "expected_ready_nodes" {
  description = "Ready nodes the cluster should have (spot-pool's fixed size). Fewer for 20 minutes alerts."
  type        = number
  default     = 3

  validation {
    condition     = var.expected_ready_nodes >= 1 && floor(var.expected_ready_nodes) == var.expected_ready_nodes
    error_message = "expected_ready_nodes must be a whole number of at least 1."
  }
}

variable "gcp_disabled_metric_namespaces" {
  description = "Google Cloud metric namespaces Datadog should not poll (each poll is billed per time series returned). gke: the Agent already reports Kubernetes metrics."
  type        = list(string)
  default     = ["gke"]
}
