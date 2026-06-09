from functools import lru_cache

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    project_name: str = "Gentian App Store"
    api_v1_str: str = "/api/v1"
    environment: str = Field(default="local", alias="ENVIRONMENT")

    # Tenant context (set by Helm per deployment)
    tenant_id: str = Field(default="demo", alias="TENANT_ID")
    tenant_namespace: str = Field(default="tenant-demo", alias="TENANT_NAMESPACE")

    # PostgreSQL — injected by ESO from kernel valueMapping
    database_url: str | None = Field(default=None, alias="DATABASE_URL")

    # OIDC — kernel Keycloak tenant realm
    oidc_issuer: str | None = Field(default=None, alias="OIDC_ISSUER")
    oidc_client_id: str | None = Field(default=None, alias="OIDC_CLIENT_ID")
    oidc_client_secret: str | None = Field(default=None, alias="OIDC_CLIENT_SECRET")
    oidc_audience: str | None = Field(default=None, alias="OIDC_AUDIENCE")

    # GitOps (optional — App Store and admin apps)
    gentian_deployments_path: str | None = Field(
        default=None, alias="GENTIAN_DEPLOYMENTS_PATH"
    )
    gentian_deployments_repo: str | None = Field(
        default=None, alias="GENTIAN_DEPLOYMENTS_REPO"
    )
    gentian_apps_repo: str = Field(
        default="https://github.com/gentian-org/gentian-apps",
        alias="GENTIAN_APPS_REPO",
    )
    gentian_apps_branch: str = Field(default="main", alias="GENTIAN_APPS_BRANCH")

    # Install mode: gitops (default) or k8s (App claims)
    install_mode: str = Field(default="gitops", alias="INSTALL_MODE")

    # Dev bypass when OIDC not configured
    auth_disabled: bool = Field(default=False, alias="AUTH_DISABLED")

    cors_origins: str = Field(default="*", alias="BACKEND_CORS_ORIGINS")

    @property
    def cors_origin_list(self) -> list[str]:
        if self.cors_origins.strip() == "*":
            return ["*"]
        return [o.strip() for o in self.cors_origins.split(",") if o.strip()]


@lru_cache
def get_settings() -> Settings:
    return Settings()
