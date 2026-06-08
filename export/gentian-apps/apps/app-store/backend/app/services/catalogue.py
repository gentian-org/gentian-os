from __future__ import annotations

from typing import Any

from app.services.k8s_client import K8sClient

PLATFORM_ANNOTATION = "gentianos.io/platform-app"


def _is_platform_app(profile: dict[str, Any]) -> bool:
    annotations = profile.get("metadata", {}).get("annotations") or {}
    return annotations.get(PLATFORM_ANNOTATION) == "true"


def build_catalogue(k8s: K8sClient, include_platform: bool = False) -> dict[str, Any]:
    catalogue = k8s.get_app_catalogue()
    status = catalogue.get("status", {})
    apps = status.get("apps", [])
    profiles = {p["metadata"]["name"]: p for p in k8s.list_app_profiles()}

    entries: list[dict[str, Any]] = []
    for entry in apps:
        name = entry.get("name")
        profile = profiles.get(name, {})
        if not include_platform and _is_platform_app(profile):
            continue
        spec = profile.get("spec", {})
        meta = profile.get("metadata", {})
        entries.append(
            {
                "name": name,
                "displayName": entry.get("displayName") or spec.get("displayName", name),
                "description": entry.get("description") or spec.get("description", ""),
                "logo": spec.get("logo"),
                "chartVersion": entry.get("chartVersion"),
                "deploymentMethod": entry.get("deploymentMethod"),
                "kernelRequirements": entry.get("kernelRequirements", []),
                "installedCount": entry.get("installedCount", 0),
                "platformApp": _is_platform_app(profile),
                "annotations": meta.get("annotations", {}),
            }
        )
    return {
        "totalApps": len(entries),
        "lastUpdated": status.get("lastUpdated"),
        "apps": entries,
    }
