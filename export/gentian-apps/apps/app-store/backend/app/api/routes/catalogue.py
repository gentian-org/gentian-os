from fastapi import APIRouter, Depends

from app.core.auth import get_current_user
from app.core.config import get_settings
from app.services.catalogue import build_catalogue
from app.services.k8s_client import K8sClient

router = APIRouter(prefix="/catalogue", tags=["catalogue"])


@router.get("/")
def get_catalogue(
    user: dict = Depends(get_current_user),
    include_platform: bool = False,
) -> dict:
    _ = user
    settings = get_settings()
    k8s = K8sClient()
    data = build_catalogue(k8s, include_platform=include_platform)
    data["catalogueRepo"] = settings.gentian_apps_repo
    data["catalogueBranch"] = settings.gentian_apps_branch
    return data


@router.get("/{profile_name}")
def get_catalogue_entry(profile_name: str, user: dict = Depends(get_current_user)) -> dict:
    _ = user
    k8s = K8sClient()
    profile = k8s.get_app_profile(profile_name)
    spec = profile.get("spec", {})
    return {
        "name": profile_name,
        "displayName": spec.get("displayName", profile_name),
        "description": spec.get("description", ""),
        "logo": spec.get("logo"),
        "chartVersion": spec.get("chart", {}).get("version"),
        "kernelRequirements": spec.get("kernelRequirements"),
    }
