variable "project_id" {
  description = "Google Cloud project ID of the lab, e.g. mamori-lab-123456."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "billing_account_id" {
  description = "Billing account that pays for the project (XXXXXX-XXXXXX-XXXXXX). Used to let tf-apply manage the budget and tf-plan read it."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[0-9A-Z]{6}-[0-9A-Z]{6}-[0-9A-Z]{6}$", var.billing_account_id))
    error_message = "billing_account_id must look like 012345-6789AB-CDEF01."
  }
}

variable "region" {
  description = "Region for the state and source buckets."
  type        = string
  default     = "asia-northeast1"
}

variable "github_repository" {
  description = "GitHub repository (owner/name) whose Actions workflows may use the lab's service accounts."
  type        = string
  default     = "codewithmuh/mamori-sre-gke"
}

variable "github_repository_id" {
  description = "Numeric id of var.github_repository (gh api repos/<owner>/<repo> --jq .id). The pool only accepts tokens whose repository_id matches."
  type        = string
  default     = "1456357674"
}

variable "github_subject_prefix" {
  description = "Prefix of the OIDC sub claim GitHub issues for this repository (gh api repos/<owner>/<repo>/actions/oidc/customization/sub --jq .sub_claim_prefix). Immutable format: repo:<owner>@<owner-id>/<repo>@<repo-id>."
  type        = string
  default     = "repo:codewithmuh@51082957/mamori-sre-gke@1456357674"
}

variable "github_apply_environment" {
  description = "GitHub environment whose jobs may impersonate tf-apply."
  type        = string
  default     = "lab"
}
