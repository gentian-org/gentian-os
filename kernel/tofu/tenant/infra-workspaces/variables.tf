variable "env" {
  description = "Environment name (dev, staging, prod)."
  type        = string
  default     = "dev"
}

variable "chart_registry" {
  description = "Base URL of the OpenDesk Helm chart registry."
  type        = string
  default     = "registry.opencode.de/bmi/opendesk/components"
}

variable "registry_username" {
  description = "Username for the OpenDesk Helm chart OCI registry."
  type        = string
  sensitive   = true
}

variable "registry_password" {
  description = "Password/token for the OpenDesk Helm chart OCI registry."
  type        = string
  sensitive   = true
}
