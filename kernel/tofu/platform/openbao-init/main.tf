terraform {
  required_providers {
    # hashicorp/vault is API-compatible with OpenBao and available in the
    # OpenTofu registry.  The openbao/openbao provider is not yet published
    # to registry.opentofu.org, so we use vault instead.
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
  }

  # State is stored locally — keep tofu/.terraform* and terraform.tfstate
  # out of Git (see .gitignore). For production, consider a remote backend
  # (e.g. S3-compatible MinIO bucket inside the cluster).
  # backend "local" {}
}

provider "vault" {
  # Address and token are read from VAULT_ADDR / VAULT_TOKEN env vars.
  # No port-forward needed: on MicroK8s the ClusterIP is directly routable
  # from the host.  Get it once with:
  #   kubectl get svc openbao -n openbao -o jsonpath='{.spec.clusterIP}'
  # then: export VAULT_ADDR=http://<cluster-ip>:8200
}
