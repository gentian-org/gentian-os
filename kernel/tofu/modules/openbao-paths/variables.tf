variable "env" {
  description = "Environment name (dev, staging, prod). Used as the KV path segment: secret/gentian/<env>/..."
  type        = string
}

variable "openbao_mount" {
  description = "OpenBao / Vault KV v2 mount path."
  type        = string
  default     = "secret"
}
