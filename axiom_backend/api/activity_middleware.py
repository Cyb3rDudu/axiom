"""
FastAPI middleware that records user-facing request activity for the
smart idle unload system.

Only non-trivial requests are counted — health checks and polling endpoints
are excluded so they don't keep models loaded.
"""

from starlette.middleware.base import BaseHTTPMiddleware
from starlette.types import ASGIApp


# Paths that should NOT count as user activity (health checks, polling, etc.)
EXCLUDED_PREFIXES = (
    "/health",
    "/api/system/status",
    "/api/system/gpu",
)


class ActivityTrackerMiddleware(BaseHTTPMiddleware):
    def __init__(self, app: ASGIApp):
        super().__init__(app)

    async def dispatch(self, request, call_next):
        path = request.url.path
        if not any(path.startswith(p) for p in EXCLUDED_PREFIXES):
            try:
                from services.activity_detector import mark_request
                mark_request()
            except Exception:
                pass
        return await call_next(request)
