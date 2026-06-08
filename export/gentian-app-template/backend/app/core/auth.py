from typing import Any

import httpx
import jwt
from fastapi import Depends, HTTPException, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer

from app.core.config import get_settings

_bearer = HTTPBearer(auto_error=False)
_jwks_cache: dict[str, Any] | None = None


async def _fetch_jwks(issuer: str) -> dict[str, Any]:
    global _jwks_cache
    if _jwks_cache is not None:
        return _jwks_cache
    url = issuer.rstrip("/") + "/protocol/openid-connect/certs"
    async with httpx.AsyncClient(timeout=10.0) as client:
        resp = await client.get(url)
        resp.raise_for_status()
        _jwks_cache = resp.json()
        return _jwks_cache


def _decode_token(token: str, issuer: str, audience: str | None) -> dict[str, Any]:
    jwks = httpx.get(issuer.rstrip("/") + "/protocol/openid-connect/certs", timeout=10.0).json()
    header = jwt.get_unverified_header(token)
    kid = header.get("kid")
    key = next((k for k in jwks.get("keys", []) if k.get("kid") == kid), None)
    if key is None:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Unknown signing key")
    public_key = jwt.algorithms.RSAAlgorithm.from_jwk(key)
    options = {"verify_aud": audience is not None}
    return jwt.decode(
        token,
        public_key,
        algorithms=["RS256"],
        audience=audience,
        issuer=issuer,
        options=options,
    )


async def get_current_user(
    credentials: HTTPAuthorizationCredentials | None = Depends(_bearer),
) -> dict[str, Any]:
    settings = get_settings()
    if settings.auth_disabled or not settings.oidc_issuer:
        return {"sub": f"admin-{settings.tenant_id}", "tenant": settings.tenant_id}
    if credentials is None or not credentials.credentials:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Not authenticated")
    try:
        claims = _decode_token(
            credentials.credentials,
            settings.oidc_issuer,
            settings.oidc_audience or settings.oidc_client_id,
        )
    except jwt.PyJWTError as exc:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED, detail=str(exc)
        ) from exc
    tenant = settings.tenant_id
    sub = claims.get("preferred_username") or claims.get("sub", "")
    if tenant and f"admin-{tenant}" not in str(sub) and tenant not in str(sub):
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="Wrong tenant")
    return claims
