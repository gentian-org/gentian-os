from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.api.routes import catalogue, health, tenant_apps
from app.core.config import get_settings

settings = get_settings()

app = FastAPI(
    title="Gentian App Store",
    openapi_url=f"{settings.api_v1_str}/openapi.json",
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=settings.cors_origin_list,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(health.router)
app.include_router(catalogue.router, prefix=settings.api_v1_str)
app.include_router(tenant_apps.router, prefix=settings.api_v1_str)
