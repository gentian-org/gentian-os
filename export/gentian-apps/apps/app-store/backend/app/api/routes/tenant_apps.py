from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, status

from app.core.auth import get_current_user
from app.core.config import get_settings
from app.services.gitops import DeploymentsGitOps, GitOpsError
from app.services.k8s_client import K8sClient

router = APIRouter(prefix="/tenant/apps", tags=["tenant-apps"])


def _actor(user: dict) -> str:
    return str(user.get("preferred_username") or user.get("sub") or "unknown")


def _tenant_namespace(tenant_id: str) -> str:
    return f"tenant-{tenant_id}"


@router.get("/installed")
def list_installed(user: dict = Depends(get_current_user)) -> dict:
    settings = get_settings()
    tenant = settings.tenant_id
    k8s = K8sClient()
    ns = settings.tenant_namespace or _tenant_namespace(tenant)

    installed: list[dict] = []
    try:
        tenant_cr = k8s.get_tenant(tenant)
        for app in tenant_cr.get("spec", {}).get("apps", []):
            profile = app.get("profile")
            if profile:
                installed.append({"profile": profile, "source": "tenant"})
    except Exception:
        pass

    for claim in k8s.list_apps_in_namespace(ns):
        meta = claim.get("metadata", {})
        spec = claim.get("spec", {})
        profile = spec.get("profileRef", {}).get("name")
        if not profile:
            continue
        conditions = claim.get("status", {}).get("conditions", [])
        ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
        installed.append(
            {
                "profile": profile,
                "source": "app-claim",
                "name": meta.get("name"),
                "ready": ready,
                "conditions": conditions,
            }
        )

    # dedupe by profile
    seen: set[str] = set()
    unique: list[dict] = []
    for item in installed:
        p = item["profile"]
        if p in seen:
            continue
        seen.add(p)
        unique.append(item)
    return {"tenant": tenant, "namespace": ns, "apps": unique}


@router.post("/{profile}/install")
def install_app(profile: str, user: dict = Depends(get_current_user)) -> dict:
    settings = get_settings()
    tenant = settings.tenant_id
    k8s = K8sClient()

    try:
        k8s.get_app_profile(profile)
    except Exception as exc:
        raise HTTPException(status_code=404, detail=f"AppProfile '{profile}' not found") from exc

    if settings.install_mode == "k8s":
        ns = settings.tenant_namespace or _tenant_namespace(tenant)
        claim_name = profile
        if k8s.app_claim_exists(ns, claim_name):
            return {"status": "already_installed", "mode": "k8s"}
        tenant_cr = k8s.get_tenant(tenant)
        domain = tenant_cr.get("spec", {}).get("domain") or f"{tenant}.example.local"
        k8s.create_app_claim(ns, claim_name, profile, ns, domain)
        return {"status": "installed", "mode": "k8s", "claim": claim_name}

    try:
        gitops = DeploymentsGitOps()
        result = gitops.install_app(tenant, profile, _actor(user))
    except GitOpsError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    return {"status": result, "mode": "gitops", "tenant": tenant, "profile": profile}


@router.delete("/{profile}")
def uninstall_app(profile: str, user: dict = Depends(get_current_user)) -> dict:
    settings = get_settings()
    tenant = settings.tenant_id

    if settings.install_mode == "k8s":
        ns = settings.tenant_namespace or _tenant_namespace(tenant)
        claim_name = profile
        k8s = K8sClient()
        if not k8s.app_claim_exists(ns, claim_name):
            return {"status": "not_installed", "mode": "k8s"}
        k8s.delete_app_claim(ns, claim_name)
        return {"status": "uninstalled", "mode": "k8s"}

    try:
        gitops = DeploymentsGitOps()
        result = gitops.uninstall_app(tenant, profile, _actor(user))
    except GitOpsError as exc:
        raise HTTPException(status_code=500, detail=str(exc)) from exc
    return {"status": result, "mode": "gitops", "tenant": tenant, "profile": profile}


@router.get("/{profile}/status")
def app_status(profile: str, user: dict = Depends(get_current_user)) -> dict:
    _ = user
    settings = get_settings()
    ns = settings.tenant_namespace or _tenant_namespace(settings.tenant_id)
    k8s = K8sClient()
    for claim in k8s.list_apps_in_namespace(ns):
        if claim.get("spec", {}).get("profileRef", {}).get("name") == profile:
            return {
                "profile": profile,
                "claim": claim.get("metadata", {}).get("name"),
                "conditions": claim.get("status", {}).get("conditions", []),
            }
    return {"profile": profile, "claim": None, "conditions": []}
