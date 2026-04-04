"""
OpenAI-compatible chat completions endpoint for Axiom document Q&A.

Exposes ``POST /v1/chat/completions`` that accepts the standard OpenAI
request format and returns a standard ChatCompletion response, plus an
extra ``sources`` field with document provenance.

Also provides API key management endpoints under ``/v1/api-keys``.
"""

import logging
import secrets
import time
import uuid
from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session

from api.schemas import (
    APIKeyResponse,
    OpenAIChatChoice,
    OpenAIChatMessage,
    OpenAIChatRequest,
    OpenAIChatResponse,
    OpenAIUsage,
)
from auth.dependencies import get_current_user_from_bearer
from database.database import get_db
from database.models import User

logger = logging.getLogger(__name__)

router = APIRouter()


def _get_doc_search_tool():
    """Return the global DocumentSearchTool from the initialised agent controller."""
    from api.missions import agent_controller

    if agent_controller is None:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="AI components not initialised yet",
        )
    doc_search_tool = getattr(agent_controller, "document_search_tool", None)
    if doc_search_tool is None:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail="Document search is not available",
        )
    return doc_search_tool


def _make_model_dispatcher(user: User):
    """Create a per-request ModelDispatcher configured with the user's API keys."""
    from ai_researcher.agentic_layer.model_dispatcher import ModelDispatcher
    from ai_researcher.user_context import set_current_user

    set_current_user(user)
    user_settings = user.settings or {}

    return ModelDispatcher(user_settings=user_settings)


# ── Chat Completions ────────────────────────────────────────────────────


@router.post("/v1/chat/completions", response_model=OpenAIChatResponse)
async def chat_completions(
    request: OpenAIChatRequest,
    current_user: User = Depends(get_current_user_from_bearer),
    db: Session = Depends(get_db),
):
    """OpenAI-compatible chat completion backed by Axiom's RAG pipeline."""

    if request.stream:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="Streaming is not supported yet",
        )

    # Extract last user message
    user_messages = [m for m in request.messages if m.role == "user"]
    if not user_messages:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="At least one user message is required",
        )
    last_user_message = user_messages[-1].content

    # Build conversation history as (user, assistant) tuples
    conversation_history = []
    msgs = request.messages
    i = 0
    while i < len(msgs) - 1:
        if msgs[i].role == "user" and i + 1 < len(msgs) and msgs[i + 1].role == "assistant":
            conversation_history.append((msgs[i].content, msgs[i + 1].content))
            i += 2
        else:
            i += 1

    # Build dependencies
    doc_search_tool = _get_doc_search_tool()
    model_dispatcher = _make_model_dispatcher(current_user)

    from ai_researcher.rag_chat import rag_chat_response

    result = await rag_chat_response(
        user_message=last_user_message,
        conversation_history=conversation_history,
        document_group_id=request.document_group_id,
        model_dispatcher=model_dispatcher,
        doc_search_tool=doc_search_tool,
        temperature=request.temperature or 0.7,
        max_tokens=request.max_tokens or 2000,
    )

    return OpenAIChatResponse(
        id=f"chatcmpl-{uuid.uuid4().hex[:24]}",
        created=int(time.time()),
        model="axiom-rag",
        choices=[
            OpenAIChatChoice(
                index=0,
                message=OpenAIChatMessage(role="assistant", content=result["content"]),
                finish_reason="stop",
            )
        ],
        usage=OpenAIUsage(**result.get("usage", {})),
        sources=result.get("sources", []),
    )


# ── API Key Management ──────────────────────────────────────────────────


@router.post("/v1/api-keys", response_model=APIKeyResponse)
async def generate_api_key(
    current_user: User = Depends(get_current_user_from_bearer),
    db: Session = Depends(get_db),
):
    """Generate a new API key for the current user (replaces any existing key)."""
    new_key = f"ax-{secrets.token_hex(32)}"
    current_user.api_key = new_key
    db.commit()
    db.refresh(current_user)

    return APIKeyResponse(
        api_key=new_key,
        created=True,
        masked_key=f"ax-...{new_key[-8:]}",
    )


@router.get("/v1/api-keys", response_model=APIKeyResponse)
async def get_api_key_status(
    current_user: User = Depends(get_current_user_from_bearer),
    db: Session = Depends(get_db),
):
    """Return masked API key info (or null if none exists)."""
    if current_user.api_key:
        return APIKeyResponse(
            api_key=None,
            created=False,
            masked_key=f"ax-...{current_user.api_key[-8:]}",
        )
    return APIKeyResponse(api_key=None, created=False, masked_key=None)


@router.delete("/v1/api-keys", response_model=APIKeyResponse)
async def revoke_api_key(
    current_user: User = Depends(get_current_user_from_bearer),
    db: Session = Depends(get_db),
):
    """Revoke the current user's API key."""
    current_user.api_key = None
    db.commit()
    return APIKeyResponse(api_key=None, created=False, masked_key=None)
