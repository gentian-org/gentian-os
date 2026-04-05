variable "realm_id" {
  description = "Keycloak realm ID (name) that contains both client_id and the trusted audience."
  type        = string
}

variable "client_id" {
  description = "Keycloak internal UUID of the client that will receive the audience mapper."
  type        = string
}

variable "included_client_audience" {
  description = "clientId (string) to include as an audience claim in the access token."
  type        = string
}

variable "add_to_id_token" {
  description = "Whether to add the audience claim to the ID token."
  type        = bool
  default     = false
}

variable "add_to_access_token" {
  description = "Whether to add the audience claim to the access token."
  type        = bool
  default     = true
}
