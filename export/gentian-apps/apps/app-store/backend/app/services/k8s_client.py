from __future__ import annotations

from typing import Any

from kubernetes import client, config
from kubernetes.client.rest import ApiException

GROUP = "gentianos.io"
VERSION = "v1alpha1"


class K8sClient:
    def __init__(self) -> None:
        try:
            config.load_incluster_config()
        except config.ConfigException:
            config.load_kube_config()
        self._custom = client.CustomObjectsApi()
        self._core = client.CoreV1Api()

    def get_app_catalogue(self) -> dict[str, Any]:
        return self._custom.get_cluster_custom_object(
            GROUP, VERSION, "appcatalogues", "default"
        )

    def get_app_profile(self, name: str) -> dict[str, Any]:
        return self._custom.get_cluster_custom_object(
            GROUP, VERSION, "appprofiles", name
        )

    def list_app_profiles(self) -> list[dict[str, Any]]:
        result = self._custom.list_cluster_custom_object(GROUP, VERSION, "appprofiles")
        return result.get("items", [])

    def get_tenant(self, name: str) -> dict[str, Any]:
        # Tenant CRs are cluster-scoped in gentian-os
        return self._custom.get_cluster_custom_object(GROUP, VERSION, "tenants", name)

    def list_apps_in_namespace(self, namespace: str) -> list[dict[str, Any]]:
        try:
            result = self._custom.list_namespaced_custom_object(
                GROUP, VERSION, namespace, "apps"
            )
            return result.get("items", [])
        except ApiException:
            return []

    def create_app_claim(
        self,
        namespace: str,
        name: str,
        profile: str,
        tenant_namespace: str,
        domain: str,
    ) -> dict[str, Any]:
        body = {
            "apiVersion": f"{GROUP}/{VERSION}",
            "kind": "App",
            "metadata": {"name": name, "namespace": namespace},
            "spec": {
                "profileRef": {"name": profile},
                "tenantNamespace": tenant_namespace,
                "domain": domain,
            },
        }
        return self._custom.create_namespaced_custom_object(
            GROUP, VERSION, namespace, "apps", body
        )

    def delete_app_claim(self, namespace: str, name: str) -> None:
        self._custom.delete_namespaced_custom_object(
            GROUP, VERSION, namespace, "apps", name
        )

    def app_claim_exists(self, namespace: str, name: str) -> bool:
        try:
            self._custom.get_namespaced_custom_object(
                GROUP, VERSION, namespace, "apps", name
            )
            return True
        except ApiException as exc:
            if exc.status == 404:
                return False
            raise
