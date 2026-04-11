variable "domain" {
  description = "Base domain for all service FQDNs (e.g. desk.gentian.org). Used in OIDC redirect/logout URLs."
  type        = string
  default     = "desk.gentian.org"
}

variable "env" {
  description = "Environment name (dev, staging, prod). Affects OpenBao secret paths and service DNS names."
  type        = string
  default     = "dev"
}

variable "realm" {
  description = "Keycloak realm that contains the opendesk OIDC clients."
  type        = string
  default     = "opendesk"
}

variable "keycloak_url" {
  description = "Override the Keycloak URL. Leave empty to use the computed in-cluster DNS name."
  type        = string
  default     = ""
}
