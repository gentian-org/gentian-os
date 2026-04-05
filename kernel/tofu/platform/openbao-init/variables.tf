variable "environments" {
  description = "List of environments to create policies and roles for."
  type        = list(string)
  default     = ["dev"]
}

variable "eso_service_account" {
  description = "Kubernetes ServiceAccount name used by External Secrets Operator."
  type        = string
  default     = "external-secrets"
}

variable "eso_namespace" {
  description = "Kubernetes namespace where ESO is deployed."
  type        = string
  default     = "external-secrets"
}

variable "kubernetes_host" {
  description = "Kubernetes API server address (used by OpenBao Kubernetes auth backend)."
  type        = string
  default     = "https://kubernetes.default.svc"
}

variable "token_ttl" {
  description = "TTL (in seconds) for tokens issued to ESO via the Kubernetes auth role."
  type        = number
  default     = 3600
}
