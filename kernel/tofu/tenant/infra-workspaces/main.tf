# =============================================================================
# Tenant Infra Workspaces — provider and backend configuration
# =============================================================================
# Managed by the Tofu Controller (Terraform CR in tofu-system).
# Providers:
#   - hashicorp/vault: reads secrets from OpenBao via Kubernetes auth
#   - helm: deploys Pattern-B Helm releases with set_sensitive
# Backend: S3-compatible MinIO running inside the cluster
# =============================================================================

terraform {
  required_providers {
    vault = {
      source  = "hashicorp/vault"
      version = "~> 4.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.0"
    }
  }

  backend "s3" {
    bucket = "tofu-state"
    key    = "tenant/infra-workspaces.tfstate"

    # MinIO endpoint (in-cluster)
    endpoint = "http://minio-dev.gentian-infra-dev.svc.cluster.local:9000"
    region   = "us-east-1" # dummy — MinIO ignores region

    # Path-style access required for MinIO
    force_path_style = true

    # Skip AWS-specific validation not applicable to MinIO
    skip_credentials_validation = true
    skip_metadata_api_check     = true
    skip_region_validation      = true
  }
}

# OpenBao (API-compatible with HashiCorp Vault) via Kubernetes auth.
# The runner pod's ServiceAccount token is exchanged for a Vault token
# using the "tofu-runner" Kubernetes auth role (bound to tf-runner SA in tofu-system).
provider "vault" {
  address = "http://openbao.openbao.svc.cluster.local:8200"

  # Kubernetes auth: exchange the runner pod's SA token for an OpenBao token.
  # auth_login is the portable form that works with all vault provider versions.
  auth_login {
    path = "auth/kubernetes/login"
    parameters = {
      role = "tofu-runner"
      jwt  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
    }
  }
}

# Kubernetes provider using in-cluster credentials (same as helm provider).
# Required for managing raw Kubernetes resources (ConfigMaps, etc.) alongside
# Helm releases.
provider "kubernetes" {
  host                   = "https://kubernetes.default.svc"
  token                  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
  cluster_ca_certificate = file("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
}

# Helm provider using in-cluster Kubernetes API credentials.
# The runner pod has a SA token and CA cert mounted at standard paths.
# Registry credentials are passed in via Terraform variables (from a K8s Secret
# referenced in the Terraform CR spec.vars).
provider "helm" {
  kubernetes {
    host                   = "https://kubernetes.default.svc"
    token                  = file("/var/run/secrets/kubernetes.io/serviceaccount/token")
    cluster_ca_certificate = file("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
  }

  registry {
    url      = "oci://registry.opencode.de"
    username = var.registry_username
    password = var.registry_password
  }
}

