# Only project_id and billing_account_id lack defaults; CI sets both.

variable "project_id" {
  description = "Google Cloud project ID of the lab, e.g. mamori-lab-123456."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "billing_account_id" {
  description = "Billing account that pays for the project (XXXXXX-XXXXXX-XXXXXX). The budget is created on it."
  type        = string
  # The PR plan is posted as a comment on a public repository.
  sensitive = true

  validation {
    condition     = can(regex("^[0-9A-Z]{6}-[0-9A-Z]{6}-[0-9A-Z]{6}$", var.billing_account_id))
    error_message = "billing_account_id must look like 012345-6789AB-CDEF01."
  }
}

variable "zone" {
  description = "Zone of the cluster. The region of every regional resource is derived from it."
  type        = string
  default     = "asia-northeast1-a"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+-[a-z]$", var.zone))
    error_message = "zone must look like asia-northeast1-a."
  }
}

variable "frontend_hostname" {
  description = "Public hostname of the shop. Its DNS is hosted outside Google Cloud; see the dns_records_to_add output."
  type        = string
  default     = "mamori.codewithmuh.com"
}

variable "master_authorized_networks" {
  description = "CIDR blocks allowed to reach the public control-plane endpoint, e.g. your IP as x.x.x.x/32. Empty means any address can connect (IAM still applies); see infra/README.md."
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  default = []

  validation {
    condition = alltrue([
      for n in var.master_authorized_networks : can(cidrhost(n.cidr_block, 0)) && n.cidr_block != "0.0.0.0/0"
    ])
    error_message = "Each cidr_block must be a valid IPv4 CIDR other than 0.0.0.0/0 (use an empty list for that)."
  }
}

variable "armor_rate_limit_per_minute" {
  description = "Requests per minute one client IP may send to the frontend before Cloud Armor answers 429."
  type        = number
  default     = 600

  validation {
    condition     = var.armor_rate_limit_per_minute >= 60 && floor(var.armor_rate_limit_per_minute) == var.armor_rate_limit_per_minute
    error_message = "armor_rate_limit_per_minute must be a whole number of at least 60."
  }
}

variable "monthly_budget_usd" {
  description = "Monthly budget in USD for this project, measured before credits. Keep it below the remaining trial credit."
  type        = number
  default     = 200

  validation {
    condition     = var.monthly_budget_usd > 0 && floor(var.monthly_budget_usd) == var.monthly_budget_usd
    error_message = "monthly_budget_usd must be a positive whole number."
  }
}

variable "spend_guard_threshold" {
  description = "Fraction of the budget at which the spend guard scales the node pool to zero (1.0 = 100 %). Lower it briefly to test the guard."
  type        = number
  default     = 1.0

  validation {
    condition     = var.spend_guard_threshold > 0 && var.spend_guard_threshold <= 2
    error_message = "spend_guard_threshold must be in (0, 2]."
  }
}

variable "spend_guard_dry_run" {
  description = "When true, the spend guard only logs what it would do."
  type        = bool
  default     = false
}
