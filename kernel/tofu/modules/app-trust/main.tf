terraform {
  required_providers {
    keycloak = {
      source  = "mrparkers/keycloak"
      version = "~> 4.0"
    }
  }
}

resource "keycloak_openid_audience_protocol_mapper" "this" {
  realm_id  = var.realm_id
  client_id = var.client_id
  name      = "${var.included_client_audience}-audience"

  included_client_audience = var.included_client_audience

  add_to_id_token     = var.add_to_id_token
  add_to_access_token = var.add_to_access_token
}
