from fastapi import APIRouter, Depends

from app.core.auth import get_current_user
from app.core.config import get_settings

router = APIRouter(prefix="/items", tags=["items"])


@router.get("/")
def list_items(user: dict = Depends(get_current_user)) -> dict:
    settings = get_settings()
    return {
        "tenant": settings.tenant_id,
        "user": user.get("sub"),
        "items": [],
        "message": "Replace with your app domain logic.",
    }
