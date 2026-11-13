variable "oncall_user_email" {
  description = "Login email of the existing PagerDuty user who goes on call (the account owner). Personal data: pass it as TF_VAR_oncall_user_email, never commit it."
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.oncall_user_email))
    error_message = "oncall_user_email must be an email address."
  }
}

variable "time_zone" {
  description = "Time zone of the schedule, as a full IANA name (Asia/Tokyo, or Etc/UTC rather than UTC). Shift times are fixed in UTC in oncall.tf."
  type        = string
  default     = "Asia/Tokyo"

  validation {
    condition     = can(regex("^[A-Za-z_]+(/[A-Za-z0-9_+-]+)+$", var.time_zone))
    error_message = "time_zone must be a full IANA time zone name such as Asia/Tokyo or Etc/UTC."
  }
}
