variable "tenant_name" {
  description = "Tenant name (e.g. 'acme-corp'). Used to scope the OpenBao policy and auth role."
  type        = string
}

variable "kubernetes_auth_path" {
  description = "Path of the Kubernetes auth method in OpenBao."
  type        = string
  default     = "kubernetes"
}

variable "token_ttl" {
  description = "TTL (in seconds) for tokens issued to tenant pods."
  type        = number
  default     = 3600
}
