from fastapi import APIRouter, Depends, HTTPException, status, BackgroundTasks
from fastapi.responses import JSONResponse
from sqlalchemy.orm import Session
from typing import List, Dict, Any, Optional
from datetime import datetime
import uuid
import logging
import json
import asyncio

logger = logging.getLogger(__name__)

from database.database import get_db
from database import models, crud
from api import schemas
from auth.dependencies import get_current_user_from_cookie
from services.document_service import DocumentService
from services.reference_service import ReferenceService
from services.structured_bibliography import (
    StructuredBibliographyService,
    ensure_unique_entry_key,
    slugify_entry_key,
)
from services.chat_title_service import ChatTitleService
from ai_researcher.agentic_layer.controller.writing_controller import WritingController
from ai_researcher.agentic_layer.agents.simplified_writing_agent import SimplifiedWritingAgent
from ai_researcher.user_context import set_current_user
from ai_researcher.dynamic_config import get_writing_mode_doc_results, get_writing_mode_web_results
from config.paths import REFERENCE_DOC_PATH

router = APIRouter(prefix="/api/writing", tags=["writing"])


@router.get("/sessions", response_model=List[schemas.WritingSessionWithChat])
async def get_writing_sessions(
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get all writing sessions for the current user."""
    
    # Get all writing sessions for the user through chat ownership
    writing_sessions = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.Chat.user_id == current_user.id
    ).order_by(models.WritingSession.updated_at.desc()).all()
    
    # Convert to response model with chat information
    result = []
    for session in writing_sessions:
        chat = db.query(models.Chat).filter(models.Chat.id == session.chat_id).first()
        
        # Get document group name if exists
        doc_group_name = None
        if session.document_group_id:
            doc_group = db.query(models.DocumentGroup).filter(
                models.DocumentGroup.id == session.document_group_id
            ).first()
            if doc_group:
                doc_group_name = doc_group.name
        
        session_data = schemas.WritingSessionWithChat(
            id=session.id,
            name=chat.title if chat else "Untitled Session",
            chat_id=session.chat_id,
            document_group_id=session.document_group_id,
            document_group_name=doc_group_name,
            web_search_enabled=session.use_web_search,
            current_draft_id=session.current_draft_id,
            settings=session.settings,
            created_at=session.created_at,
            updated_at=session.updated_at
        )
        result.append(session_data)
    
    return result

@router.post("/chats", response_model=schemas.Chat)
async def create_writing_chat(
    chat_data: schemas.ChatCreate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Create a new chat specifically for writing sessions."""
    try:
        chat_id = str(uuid.uuid4())
        
        # Create a writing-specific chat
        db_chat = models.Chat(
            id=chat_id,
            user_id=current_user.id,
            title=chat_data.title,
            chat_type="writing",  # Set chat type to writing
            created_at=datetime.utcnow(),
            updated_at=datetime.utcnow()
        )
        
        db.add(db_chat)
        db.commit()
        db.refresh(db_chat)
        
        logger.info(f"Created new writing chat {chat_id} for user {current_user.username}")
        return db_chat
        
    except Exception as e:
        logger.error(f"Error creating writing chat: {e}")
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail=f"Failed to create writing chat: {str(e)}"
        )

@router.post("/sessions", response_model=schemas.WritingSession)
async def create_writing_session(
    session_data: schemas.WritingSessionCreate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Create a new writing session."""
    
    # Verify the chat belongs to the current user and is a writing chat
    chat = db.query(models.Chat).filter(
        models.Chat.id == session_data.chat_id,
        models.Chat.user_id == current_user.id,
        models.Chat.chat_type == "writing"
    ).first()
    
    if not chat:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing chat not found or access denied"
        )
    
    # Verify document group belongs to user if specified
    if session_data.document_group_id:
        doc_group = db.query(models.DocumentGroup).filter(
            models.DocumentGroup.id == session_data.document_group_id,
            models.DocumentGroup.user_id == current_user.id
        ).first()
        
        if not doc_group:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail="Document group not found or access denied"
            )
    
    # Check if writing session already exists for this chat
    existing_session = db.query(models.WritingSession).filter(
        models.WritingSession.chat_id == session_data.chat_id
    ).first()
    
    if existing_session:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="Writing session already exists for this chat"
        )
    
    # Validate every doc group in the list belongs to the user.
    if session_data.document_group_ids:
        bad = (
            db.query(models.DocumentGroup.id)
            .filter(
                models.DocumentGroup.id.in_(session_data.document_group_ids),
                models.DocumentGroup.user_id == current_user.id,
            )
            .count()
        )
        if bad < len({g for g in session_data.document_group_ids if g}):
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail="One or more document groups not found or access denied",
            )

    # Portfolio opt-out (#68): keyword in the chat title → writes
    # settings.portfolio_enabled=False at creation time so later gates
    # don't need to re-inspect the title. Mirrors the mission-side
    # detect_portfolio_optout pattern.
    from ai_researcher.agentic_layer.controller.utils.portfolio_optout import (
        detect_portfolio_optout,
    )

    effective_settings = dict(session_data.settings or {})
    if "portfolio_enabled" not in effective_settings:
        if detect_portfolio_optout(chat.title or ""):
            effective_settings["portfolio_enabled"] = False
        else:
            effective_settings["portfolio_enabled"] = True

    # Create new writing session
    writing_session = models.WritingSession(
        id=str(uuid.uuid4()),
        chat_id=session_data.chat_id,
        document_group_id=session_data.document_group_id,
        document_group_ids=session_data.document_group_ids,
        use_web_search=session_data.use_web_search,
        settings=effective_settings,
        created_at=datetime.utcnow(),
        updated_at=datetime.utcnow()
    )
    
    db.add(writing_session)
    db.commit()
    db.refresh(writing_session)
    
    return writing_session

# Writing Session Stats Endpoints

@router.get("/sessions/{session_id}/stats", response_model=schemas.WritingSessionStats)
async def get_writing_session_stats(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get usage statistics for a writing session."""
    
    # Verify writing session access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get or create stats
    stats = crud.get_or_create_writing_session_stats(db, session_id)
    
    return schemas.WritingSessionStats(
        session_id=session_id,
        total_cost=float(stats.total_cost),
        total_prompt_tokens=stats.total_prompt_tokens,
        total_completion_tokens=stats.total_completion_tokens,
        total_native_tokens=stats.total_native_tokens,
        total_web_searches=stats.total_web_searches,
        total_document_searches=stats.total_document_searches,
        created_at=stats.created_at,
        updated_at=stats.updated_at
    )

@router.post("/sessions/{session_id}/stats", response_model=schemas.WritingSessionStats)
async def update_writing_session_stats(
    session_id: str,
    stats_update: schemas.WritingSessionStatsUpdate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Update writing session statistics with delta values."""
    
    # Verify writing session access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Update stats with delta values
    updated_stats = crud.update_writing_session_stats(db, session_id, stats_update)
    
    if not updated_stats:
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail="Failed to update writing session stats"
        )
    
    # Send real-time update via WebSocket
    try:
        from api.websockets import send_writing_stats_update
        await send_writing_stats_update(session_id, {
            "total_cost": float(updated_stats.total_cost),
            "total_prompt_tokens": updated_stats.total_prompt_tokens,
            "total_completion_tokens": updated_stats.total_completion_tokens,
            "total_native_tokens": updated_stats.total_native_tokens,
            "total_web_searches": updated_stats.total_web_searches,
            "total_document_searches": updated_stats.total_document_searches
        })
        logger.debug(f"Sent stats update via WebSocket for writing session {session_id}")
    except Exception as e:
        logger.warning(f"Failed to send stats update via WebSocket: {e}")
    
    return schemas.WritingSessionStats(
        session_id=session_id,
        total_cost=float(updated_stats.total_cost),
        total_prompt_tokens=updated_stats.total_prompt_tokens,
        total_completion_tokens=updated_stats.total_completion_tokens,
        total_native_tokens=updated_stats.total_native_tokens,
        total_web_searches=updated_stats.total_web_searches,
        total_document_searches=updated_stats.total_document_searches,
        created_at=updated_stats.created_at,
        updated_at=updated_stats.updated_at
    )

@router.post("/sessions/{session_id}/stats/clear", response_model=schemas.WritingSessionStats)
async def clear_writing_session_stats(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Clear/reset all statistics for a writing session."""
    
    # Verify writing session access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Clear stats
    cleared_stats = crud.clear_writing_session_stats(db, session_id)
    
    if not cleared_stats:
        raise HTTPException(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail="Failed to clear writing session stats"
        )
    
    # Send real-time update via WebSocket
    try:
        from api.websockets import send_writing_stats_update
        await send_writing_stats_update(session_id, {
            "total_cost": 0.0,
            "total_prompt_tokens": 0,
            "total_completion_tokens": 0,
            "total_native_tokens": 0,
            "total_web_searches": 0,
            "total_document_searches": 0
        })
        logger.debug(f"Sent stats clear update via WebSocket for writing session {session_id}")
    except Exception as e:
        logger.warning(f"Failed to send stats clear update via WebSocket: {e}")
    
    return schemas.WritingSessionStats(
        session_id=session_id,
        total_cost=0.0,
        total_prompt_tokens=0,
        total_completion_tokens=0,
        total_native_tokens=0,
        total_web_searches=0,
        total_document_searches=0,
        created_at=cleared_stats.created_at,
        updated_at=cleared_stats.updated_at
    )

@router.get("/sessions/{session_id}", response_model=schemas.WritingSessionWithDrafts)
async def get_writing_session(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get a writing session with its drafts."""
    
    # Get writing session and verify access through chat ownership
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get all drafts for this session
    drafts = db.query(models.Draft).filter(
        models.Draft.writing_session_id == session_id
    ).order_by(models.Draft.created_at.desc()).all()
    
    # Get current draft
    current_draft = None
    if writing_session.current_draft_id:
        current_draft = db.query(models.Draft).filter(
            models.Draft.id == writing_session.current_draft_id
        ).first()
    
    # Convert to response model
    response_data = schemas.WritingSessionWithDrafts(
        id=writing_session.id,
        chat_id=writing_session.chat_id,
        document_group_id=writing_session.document_group_id,
        use_web_search=writing_session.use_web_search,
        current_draft_id=writing_session.current_draft_id,
        settings=writing_session.settings,
        created_at=writing_session.created_at,
        updated_at=writing_session.updated_at,
        drafts=[schemas.Draft.from_orm(draft) for draft in drafts],
        current_draft=schemas.Draft.from_orm(current_draft) if current_draft else None
    )
    
    return response_data

# Draft Management Endpoints

@router.get("/sessions/{session_id}/messages", response_model=List[schemas.Message])
async def get_writing_session_messages(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get all messages for a writing session."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get chat and messages
    chat = db.query(models.Chat).filter(models.Chat.id == writing_session.chat_id).first()
    if not chat:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Chat not found"
        )
    
    # Get all messages for this chat, ordered by creation time
    messages = db.query(models.Message).filter(
        models.Message.chat_id == chat.id
    ).order_by(models.Message.created_at.asc()).all()
    
    return messages

@router.get("/sessions/{session_id}/draft", response_model=schemas.DraftWithReferences)
async def get_current_draft(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get the current draft for a writing session."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get current draft
    current_draft = None
    if writing_session.current_draft_id:
        current_draft = db.query(models.Draft).filter(
            models.Draft.id == writing_session.current_draft_id
        ).first()
    
    if not current_draft:
        # Create a new draft if none exists
        current_draft = models.Draft(
            id=str(uuid.uuid4()),
            writing_session_id=session_id,
            title="Untitled Document",
            content="",  # Start with completely blank content - no placeholder text
            version=1,
            is_current=True,
            created_at=datetime.utcnow(),
            updated_at=datetime.utcnow()
        )

        db.add(current_draft)

        # Update writing session to point to this draft
        writing_session.current_draft_id = current_draft.id
        writing_session.updated_at = datetime.utcnow()

        db.commit()
        db.refresh(current_draft)

        logger.info(f"Created new blank draft {current_draft.id} for writing session {session_id}")

        # Mission → writing handoff (#73): if the session was created
        # from a finished research mission, project its citation graph
        # + Literaturportfolio into this draft so the user doesn't have
        # to click "Aus Markdown importieren" or wait for a writer
        # turn to re-emit the references block.
        session_settings = writing_session.settings if isinstance(writing_session.settings, dict) else {}
        mission_source_id = session_settings.get("mission_source_id") if session_settings else None
        if mission_source_id:
            try:
                from services.mission_to_writing_handoff import project_mission_into_draft
                project_mission_into_draft(
                    db,
                    mission_id=mission_source_id,
                    draft=current_draft,
                    user_id=current_user.id,
                )
                # Clear the marker so re-fetches don't re-project
                session_settings.pop("mission_source_id", None)
                writing_session.settings = dict(session_settings)
                db.commit()
                db.refresh(current_draft)
            except Exception as exc:
                logger.warning(
                    "Handoff projection failed for mission=%s draft=%s: %s",
                    mission_source_id,
                    current_draft.id,
                    exc,
                )
    
    # Get references for this draft
    references = db.query(models.Reference).filter(
        models.Reference.draft_id == current_draft.id
    ).all()
    
    # Convert to response model
    response_data = schemas.DraftWithReferences(
        id=current_draft.id,
        writing_session_id=current_draft.writing_session_id,
        title=current_draft.title,
        content=current_draft.content,
        version=current_draft.version,
        is_current=current_draft.is_current,
        created_at=current_draft.created_at,
        updated_at=current_draft.updated_at,
        references=[schemas.Reference.from_orm(ref) for ref in references]
    )
    
    return response_data

@router.put("/sessions/{session_id}/draft", response_model=schemas.Draft)
async def update_current_draft(
    session_id: str,
    draft_update: schemas.DraftUpdate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Update the current draft for a writing session."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get current draft
    if not writing_session.current_draft_id:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="No current draft found"
        )
    
    current_draft = db.query(models.Draft).filter(
        models.Draft.id == writing_session.current_draft_id
    ).first()
    
    if not current_draft:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Current draft not found"
        )
    
    # Update fields
    update_data = draft_update.dict(exclude_unset=True)
    for field, value in update_data.items():
        setattr(current_draft, field, value)
    
    current_draft.updated_at = datetime.utcnow()
    
    # No need to update metadata since content is now simple markdown
    
    db.commit()
    db.refresh(current_draft)
    
    return current_draft

@router.post("/sessions/{session_id}/versions", response_model=schemas.Draft)
async def create_draft_version(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Create a new version of the current draft."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Get current draft
    if not writing_session.current_draft_id:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="No current draft found"
        )
    
    current_draft = db.query(models.Draft).filter(
        models.Draft.id == writing_session.current_draft_id
    ).first()
    
    if not current_draft:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Current draft not found"
        )
    
    # Mark current draft as not current
    current_draft.is_current = False
    
    # Create new version
    new_version = models.Draft(
        id=str(uuid.uuid4()),
        writing_session_id=session_id,
        title=current_draft.title,
        content=current_draft.content,  # Copy the content string
        version=current_draft.version + 1,
        is_current=True,
        created_at=datetime.utcnow(),
        updated_at=datetime.utcnow()
    )
    
    db.add(new_version)
    
    # Update writing session to point to new version
    writing_session.current_draft_id = new_version.id
    writing_session.updated_at = datetime.utcnow()
    
    db.commit()
    db.refresh(new_version)
    
    return new_version


# Reference Management Endpoints

@router.get("/drafts/{draft_id}/references", response_model=List[schemas.Reference])
async def get_draft_references(
    draft_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get all references for a draft."""
    
    ref_service = ReferenceService(db)
    references = await ref_service.get_references_for_draft(
        draft_id=draft_id,
        user_id=current_user.id
    )
    
    return [schemas.Reference.from_orm(ref) for ref in references]

@router.post("/drafts/{draft_id}/references", response_model=schemas.Reference)
async def create_reference(
    draft_id: str,
    reference_data: schemas.ReferenceCreate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Create a new reference for a draft."""
    
    ref_service = ReferenceService(db)
    
    if reference_data.reference_type == "web":
        reference = await ref_service.create_reference_from_web_source(
            draft_id=draft_id,
            web_url=reference_data.web_url,
            title=reference_data.citation_text,  # Using citation_text as title for now
            user_id=current_user.id
        )
    else:
        # For document references, we'll need document_id
        reference = await ref_service.create_reference_from_document_chunk(
            draft_id=draft_id,
            document_chunk_id=reference_data.document_id,
            user_id=current_user.id
        )
    
    return schemas.Reference.from_orm(reference)

@router.put("/sessions/{session_id}", response_model=schemas.WritingSession)
async def update_writing_session(
    session_id: str,
    session_update: schemas.WritingSessionUpdate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Update a writing session."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Verify document group belongs to user if being updated
    if session_update.document_group_id is not None:
        if session_update.document_group_id:  # Not empty string
            doc_group = db.query(models.DocumentGroup).filter(
                models.DocumentGroup.id == session_update.document_group_id,
                models.DocumentGroup.user_id == current_user.id
            ).first()

            if not doc_group:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail="Document group not found or access denied"
                )
    if session_update.document_group_ids is not None and session_update.document_group_ids:
        owned = (
            db.query(models.DocumentGroup.id)
            .filter(
                models.DocumentGroup.id.in_(session_update.document_group_ids),
                models.DocumentGroup.user_id == current_user.id,
            )
            .count()
        )
        if owned < len({g for g in session_update.document_group_ids if g}):
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail="One or more document groups not found or access denied",
            )
    
    # Update fields
    update_data = session_update.dict(exclude_unset=True)
    for field, value in update_data.items():
        setattr(writing_session, field, value)
    
    writing_session.updated_at = datetime.utcnow()
    
    db.commit()
    db.refresh(writing_session)
    
    return writing_session

@router.delete("/sessions/{session_id}")
async def delete_writing_session(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Delete a writing session and all its drafts."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Delete the session (cascading will handle drafts and references)
    db.delete(writing_session)
    db.commit()
    
    return {"message": "Writing session deleted successfully"}

@router.get("/sessions/by-chat/{chat_id}", response_model=schemas.WritingSessionWithDrafts)
async def get_writing_session_by_chat(
    chat_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Get writing session for a specific chat."""
    
    # Verify chat belongs to user
    chat = db.query(models.Chat).filter(
        models.Chat.id == chat_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not chat:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Chat not found or access denied"
        )
    
    # Get writing session for this chat
    writing_session = db.query(models.WritingSession).filter(
        models.WritingSession.chat_id == chat_id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="No writing session found for this chat"
        )
    
    # Get all drafts for this session
    drafts = db.query(models.Draft).filter(
        models.Draft.writing_session_id == writing_session.id
    ).order_by(models.Draft.created_at.desc()).all()
    
    # Get current draft
    current_draft = None
    if writing_session.current_draft_id:
        current_draft = db.query(models.Draft).filter(
            models.Draft.id == writing_session.current_draft_id
        ).first()
    
    # Convert to response model
    response_data = schemas.WritingSessionWithDrafts(
        id=writing_session.id,
        chat_id=writing_session.chat_id,
        document_group_id=writing_session.document_group_id,
        use_web_search=writing_session.use_web_search,
        current_draft_id=writing_session.current_draft_id,
        settings=writing_session.settings,
        created_at=writing_session.created_at,
        updated_at=writing_session.updated_at,
        drafts=[schemas.Draft.from_orm(draft) for draft in drafts],
        current_draft=schemas.Draft.from_orm(current_draft) if current_draft else None
    )
    
    return response_data

@router.put("/drafts/{draft_id}/references/{reference_id}", response_model=schemas.Reference)
async def update_reference(
    draft_id: str,
    reference_id: str,
    citation_text: str = None,
    context: str = None,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Update an existing reference."""

    ref_service = ReferenceService(db)
    reference = await ref_service.update_reference(
        reference_id=reference_id,
        citation_text=citation_text,
        context=context,
        user_id=current_user.id
    )

    return schemas.Reference.from_orm(reference)


# Structured bibliography endpoints (#51/#52). These accept the new
# structured fields (authors, year, entry_key, …) and operate on the
# citation registry directly, bypassing the legacy chunk/web source
# creation paths that go through ReferenceService.

@router.post(
    "/drafts/{draft_id}/references/structured",
    response_model=schemas.Reference,
)
async def create_structured_reference(
    draft_id: str,
    payload: schemas.StructuredReferenceCreate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Create or update a structured reference entry."""
    from services.feature_flags import structured_bibliography_enabled

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")
    service = StructuredBibliographyService(db)

    data = payload.dict()
    entry_key = (data.get("entry_key") or "").strip()
    if not entry_key:
        # Synthesize a slug from authors + year + title
        hint_parts = [
            (payload.authors[0].family if payload.authors else ""),
            str(payload.year) if payload.year else "",
            (payload.title or "")[:40],
        ]
        entry_key = slugify_entry_key(*hint_parts)
    data["entry_key"] = ensure_unique_entry_key(db, draft_id, entry_key)

    result = service.upsert_structured(
        draft_id=draft_id,
        payload=data,
        user_id=current_user.id,
        citation_text=data.get("citation_text") or "",
    )
    return schemas.Reference.from_orm(result.reference)


@router.put(
    "/drafts/{draft_id}/references/{reference_id}/structured",
    response_model=schemas.Reference,
)
async def update_structured_reference(
    draft_id: str,
    reference_id: str,
    payload: schemas.StructuredReferenceCreate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Replace a reference's structured fields. entry_key must match."""
    from services.feature_flags import structured_bibliography_enabled

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")
    service = StructuredBibliographyService(db)
    existing = service.get_reference(reference_id, current_user.id)
    if existing.draft_id != draft_id:
        raise HTTPException(status_code=404, detail="Reference not found for this draft")

    data = payload.dict()
    # Preserve the existing key unless caller explicitly changes it
    data["entry_key"] = data.get("entry_key") or existing.entry_key
    result = service.upsert_structured(
        draft_id=draft_id,
        payload=data,
        user_id=current_user.id,
        citation_text=data.get("citation_text") or existing.citation_text or "",
    )
    return schemas.Reference.from_orm(result.reference)


@router.post("/drafts/{draft_id}/references/migrate-from-markdown")
async def migrate_bibliography_from_markdown(
    draft_id: str,
    dry_run: bool = True,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Parse inline Literaturverzeichnis into structured entries (#54).

    `dry_run=true` returns the preview without persisting (the frontend
    shows the diff for user confirmation). `dry_run=false` commits the
    parsed entries via replace_draft_registry; unparsable lines are
    returned so the user can retry or enter them manually.
    """
    from services.feature_flags import structured_bibliography_enabled
    from services.bibliography_migrator import migrate_markdown_bibliography
    from services.citation_profiles import resolve_citation_profile
    from services.citation_rendering import render_entry

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")

    service = StructuredBibliographyService(db)
    draft = service._assert_draft_access(draft_id, current_user.id)

    profile = resolve_citation_profile(None, current_user.settings)
    preview = migrate_markdown_bibliography(
        draft.content or "",
        profile_hint=profile.citation_mode if profile else None,
    )

    if dry_run or not preview.entries:
        return preview.to_dict()

    pid = profile.id if profile else "numbered"
    service.replace_draft_registry(
        draft_id=draft_id,
        entries=[e.to_dict() for e in preview.entries],
        user_id=current_user.id,
        render_citation=lambda e, _pid=pid: render_entry(e, _pid),
    )

    return preview.to_dict()


# Writing-mode Portfolio generation (#61/#65). In-process registry so
# concurrent "generate" clicks on the same draft return 409 instead of
# racing to a duplicate agent call.
_portfolio_generation_in_flight: Dict[str, asyncio.Task] = {}
_portfolio_generation_lock = asyncio.Lock()


@router.post("/drafts/{draft_id}/portfolio/generate")
async def generate_writing_portfolio(
    draft_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Run WritingPortfolioManager for a draft and return the PortfolioOutput.

    Gated on the structured-bibliography flag and the per-session
    portfolio opt-out. Concurrent generations on the same draft return
    409. Errors propagate so the UI can badge failure.
    """
    from services.feature_flags import structured_bibliography_enabled
    from ai_researcher.agentic_layer.controller.writing_portfolio_manager import (
        WritingPortfolioManager,
    )

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")

    draft = (
        db.query(models.Draft)
        .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(models.Draft.id == draft_id, models.Chat.user_id == current_user.id)
        .first()
    )
    if draft is None:
        raise HTTPException(status_code=404, detail="Draft not found or access denied")

    writing_session = (
        db.query(models.WritingSession)
        .filter(models.WritingSession.id == draft.writing_session_id)
        .first()
    )

    # Atomic check-and-register: hold the lock across both the
    # duplicate check and the task registration so two concurrent
    # callers can't both see "no in-flight" and race the agent run.
    # The same lock also coordinates the finalize endpoint — hold
    # here, finalize acquires the same lock before running the manager.
    task = asyncio.current_task()
    async with _portfolio_generation_lock:
        existing = _portfolio_generation_in_flight.get(draft_id)
        if existing is not None and not existing.done():
            raise HTTPException(status_code=409, detail="Portfolio generation already in flight for this draft")
        _portfolio_generation_in_flight[draft_id] = task

    # Resolve the writing controller's model dispatcher — same one the
    # chat endpoint uses. Keeps generation consistent with the agent's
    # preferred routing.
    writing_controller = WritingController(current_user)
    manager = WritingPortfolioManager(writing_controller.model_dispatcher, db)

    try:
        output = await manager.run_if_enabled(
            draft=draft,
            writing_session=writing_session,
            user=current_user,
            trigger="manual",
        )
    finally:
        async with _portfolio_generation_lock:
            if _portfolio_generation_in_flight.get(draft_id) is task:
                _portfolio_generation_in_flight.pop(draft_id, None)

    if output is None:
        raise HTTPException(
            status_code=409,
            detail="Portfolio not generated — check flag, opt-out, or empty bibliography",
        )

    return output.model_dump(mode="json")


@router.post("/sessions/{session_id}/finalize")
async def finalize_writing_session(
    session_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Session-close hook: run the portfolio on the current draft (#68).

    Idempotent: if portfolio_output is already populated for the current
    draft, returns it as-is. Otherwise runs the manager (subject to the
    usual gates — flag, opt-out, non-empty bibliography). Designed for
    the frontend to call on session unmount / explicit 'Fertigstellen'.
    """
    from ai_researcher.agentic_layer.controller.writing_portfolio_manager import (
        WritingPortfolioManager,
    )

    writing_session = (
        db.query(models.WritingSession)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(
            models.WritingSession.id == session_id,
            models.Chat.user_id == current_user.id,
        )
        .first()
    )
    if writing_session is None:
        raise HTTPException(status_code=404, detail="Writing session not found or access denied")

    if not writing_session.current_draft_id:
        return {"status": "no_draft"}

    draft = (
        db.query(models.Draft)
        .filter(models.Draft.id == writing_session.current_draft_id)
        .first()
    )
    if draft is None:
        return {"status": "no_draft"}

    # Idempotent: if the current draft already has a portfolio, return it.
    if draft.portfolio_output:
        return {"status": "already_generated", "portfolio_output": draft.portfolio_output}

    # Coordinate with the generate endpoint's in-flight registry so a
    # user who hammers both Generate and session-close doesn't trigger
    # two agent runs. If generation is already in flight, report
    # as-is — the other caller will write the column shortly.
    task = asyncio.current_task()
    async with _portfolio_generation_lock:
        existing = _portfolio_generation_in_flight.get(draft.id)
        if existing is not None and not existing.done():
            return {"status": "already_generating"}
        _portfolio_generation_in_flight[draft.id] = task

    writing_controller = WritingController(current_user)
    manager = WritingPortfolioManager(writing_controller.model_dispatcher, db)
    try:
        output = await manager.run_if_enabled(
            draft=draft,
            writing_session=writing_session,
            user=current_user,
            trigger="session_close",
        )
    finally:
        async with _portfolio_generation_lock:
            if _portfolio_generation_in_flight.get(draft.id) is task:
                _portfolio_generation_in_flight.pop(draft.id, None)
    if output is None:
        return {"status": "skipped"}
    return {"status": "generated", "portfolio_output": output.model_dump(mode="json")}


@router.delete("/drafts/{draft_id}/portfolio")
async def clear_writing_portfolio(
    draft_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Null the portfolio_output column so the UI can force regeneration."""
    from services.feature_flags import structured_bibliography_enabled

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")
    draft = (
        db.query(models.Draft)
        .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(models.Draft.id == draft_id, models.Chat.user_id == current_user.id)
        .first()
    )
    if draft is None:
        raise HTTPException(status_code=404, detail="Draft not found or access denied")
    draft.portfolio_output = None
    draft.updated_at = datetime.utcnow()
    db.commit()
    return {"status": "cleared"}


@router.delete("/drafts/{draft_id}/references/{reference_id}")
async def delete_structured_reference(
    draft_id: str,
    reference_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Delete a reference. Cascades to any citation_entries rows."""
    from services.feature_flags import structured_bibliography_enabled

    if not structured_bibliography_enabled(current_user.settings):
        raise HTTPException(status_code=404, detail="Structured bibliography is not enabled")
    service = StructuredBibliographyService(db)
    existing = service.get_reference(reference_id, current_user.id)
    if existing.draft_id != draft_id:
        raise HTTPException(status_code=404, detail="Reference not found for this draft")
    service.delete_reference(reference_id, current_user.id)
    return {"status": "deleted"}


# Writing task registry (#45). Each in-flight background chat task is
# keyed on its task_id here so the DELETE endpoint can cancel it. The
# registry is per-process — fine for single-backend deployments, and
# acceptable for multi-replica since each WS-connected user hits one
# replica at a time. Protected by an asyncio.Lock so the register /
# cancel / unregister ops are safe across concurrent chat calls.
_writing_task_registry: Dict[str, asyncio.Task] = {}
_writing_task_registry_lock = asyncio.Lock()


async def _register_writing_task(task_id: str, task: asyncio.Task) -> None:
    async with _writing_task_registry_lock:
        _writing_task_registry[task_id] = task


async def _unregister_writing_task(task_id: str) -> None:
    async with _writing_task_registry_lock:
        _writing_task_registry.pop(task_id, None)


async def _cancel_writing_task(task_id: str) -> bool:
    async with _writing_task_registry_lock:
        task = _writing_task_registry.get(task_id)
        if task is None or task.done():
            return False
        task.cancel()
        return True


# Enhanced Writing Chat Endpoint - Non-blocking version

@router.post("/enhanced-chat-stream")
async def enhanced_writing_chat_stream(
    request: schemas.EnhancedWritingChatRequest,
    background_tasks: BackgroundTasks,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """
    Non-blocking version of enhanced chat that processes in background and streams via WebSocket.
    Returns immediately with a task ID.
    """
    # Verify that the draft exists and the user has access to it.
    draft = db.query(models.Draft).join(
        models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id
    ).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.Draft.id == request.draft_id,
        models.Chat.user_id == current_user.id
    ).first()

    if not draft:
        raise HTTPException(status_code=404, detail="Draft not found or access denied")

    # Generate a task ID for tracking
    task_id = str(uuid.uuid4())
    
    # Get the writing session
    writing_session = draft.writing_session
    session_id = writing_session.id
    
    # Check if this is a regeneration by looking for an existing user message with the same content
    # that's recent (within last 10 messages) to avoid false positives
    recent_messages = db.query(models.Message).filter(
        models.Message.chat_id == draft.writing_session.chat_id
    ).order_by(models.Message.created_at.desc()).limit(10).all()
    
    existing_user_message = None
    for msg in recent_messages:
        if msg.role == "user" and msg.content == request.message:
            existing_user_message = msg
            break
    
    if existing_user_message:
        # This is a regeneration - reuse the existing user message
        user_message = existing_user_message
        logger.info(f"Detected regeneration - reusing existing user message {existing_user_message.id} for chat {draft.writing_session.chat_id}")
        
        # Clean up any orphaned assistant messages after this user message
        orphaned_messages = db.query(models.Message).filter(
            models.Message.chat_id == draft.writing_session.chat_id,
            models.Message.created_at > existing_user_message.created_at
        ).all()
        
        if orphaned_messages:
            logger.info(f"Cleaning up {len(orphaned_messages)} orphaned messages for regeneration")
            for msg in orphaned_messages:
                db.delete(msg)
            db.commit()
    else:
        # New message - save it
        user_message = models.Message(
            id=str(uuid.uuid4()),
            chat_id=draft.writing_session.chat_id,
            role="user",
            content=request.message,
            created_at=datetime.utcnow()
        )
        db.add(user_message)
        db.commit()
    
    # Don't send any immediate updates - let the agent handle all status updates

    # Spawn via asyncio.create_task so we get a Task handle we can
    # register and later cancel from the DELETE endpoint. FastAPI's
    # own BackgroundTasks runs post-response but gives no handle; this
    # path runs outside the request lifecycle and is owned entirely
    # by the registry.
    bg_coro = process_writing_chat_in_background(
        task_id=task_id,
        request=request,
        draft_id=draft.id,
        session_id=session_id,
        chat_id=draft.writing_session.chat_id,
        user_id=current_user.id,
        user_message_id=user_message.id,
    )
    bg_task = asyncio.create_task(bg_coro, name=f"writing-chat:{task_id}")
    await _register_writing_task(task_id, bg_task)
    
    # Return immediately with task ID
    return JSONResponse(
        status_code=202,  # Accepted
        content={
            "task_id": task_id,
            "status": "processing",
            "message": "Your request is being processed. Updates will be streamed via WebSocket."
        }
    )

async def process_writing_chat_in_background(
    task_id: str,
    request: schemas.EnhancedWritingChatRequest,
    draft_id: str,
    session_id: str,
    chat_id: str,
    user_id: int,
    user_message_id: str
):
    """
    Process writing chat in background and send updates via WebSocket.
    """
    from api.websockets import send_agent_status_update, send_draft_content_update
    from database.database import SessionLocal
    
    db = SessionLocal()
    try:
        # Get user and draft
        current_user = db.query(models.User).filter(models.User.id == user_id).first()
        draft = db.query(models.Draft).filter(models.Draft.id == draft_id).first()
        writing_session = db.query(models.WritingSession).filter(models.WritingSession.id == session_id).first()
        
        if not current_user or not draft or not writing_session:
            await send_streaming_chunk_update(session_id, "\n❌ Error: Session data not found.\n")
            return
        
        # Set user context
        set_current_user(current_user)
        
        # Determine settings (same logic as original endpoint)
        document_group_id = request.document_group_id
        if document_group_id == "" or document_group_id == "none":
            document_group_id = None
        elif document_group_id is None:
            document_group_id = writing_session.document_group_id

        # Multi-group scope. Request overrides session row; if neither is
        # set, fall back to the singular primary group.
        from services.doc_group_resolution import (
            normalize_group_ids, resolve_group_ids_to_doc_ids
        )
        request_group_ids = request.document_group_ids
        if request_group_ids is None:
            request_group_ids = writing_session.document_group_ids
        effective_group_ids = normalize_group_ids(document_group_id, request_group_ids)

        use_web_search = request.use_web_search
        if use_web_search is None:
            use_web_search = writing_session.use_web_search
        
        # Get the WritingController
        writing_controller = await WritingController.get_instance(current_user)
        agent = SimplifiedWritingAgent(model_dispatcher=writing_controller.model_dispatcher)
        
        # Get chat history (excluding the message we just added)
        chat_history = db.query(models.Message).filter(
            models.Message.chat_id == chat_id,
            models.Message.id != user_message_id
        ).order_by(models.Message.created_at.asc()).all()
        
        # Convert to list of dictionaries for the agent
        chat_history_messages = [{"role": msg.role, "content": msg.content} for msg in chat_history]
        
        # Prepare context
        custom_system_prompt = None
        if current_user.settings and isinstance(current_user.settings, dict):
            writing_settings = current_user.settings.get("writing_settings", {})
            if isinstance(writing_settings, dict):
                custom_system_prompt = writing_settings.get("custom_system_prompt")
        
        user_research_params = current_user.settings.get("research_parameters", {}) if current_user.settings else {}
        user_search_settings = current_user.settings.get("search", {}) if current_user.settings else {}
        
        if request.deep_search:
            default_iterations = user_research_params.get("writing_deep_search_iterations", 3)
            default_queries = user_research_params.get("writing_deep_search_queries", 5)
        else:
            default_iterations = user_research_params.get("writing_search_max_iterations", 1)
            default_queries = user_research_params.get("writing_search_max_queries", 3)
        
        # Fetch document group names for all selected groups. Used both as
        # a human-readable hint in the agent context and for resolving the
        # union of doc IDs that bound the vector search.
        document_group_name = None
        document_group_names: List[str] = []
        filter_doc_ids: List[str] = []
        if effective_group_ids:
            group_rows = (
                db.query(models.DocumentGroup)
                .filter(
                    models.DocumentGroup.id.in_(effective_group_ids),
                    models.DocumentGroup.user_id == current_user.id,
                )
                .all()
            )
            by_id = {str(g.id): g for g in group_rows}
            document_group_names = [
                by_id[gid].name for gid in effective_group_ids if gid in by_id
            ]
            if document_group_names:
                document_group_name = " + ".join(document_group_names)
            filter_doc_ids = resolve_group_ids_to_doc_ids(
                db, current_user.id, effective_group_ids
            )
            logger.info(
                f"Writing-chat scope: {len(effective_group_ids)} group(s) → "
                f"{len(filter_doc_ids)} doc(s) — {document_group_names}"
            )

        # Resolve citation profile for this writing session. Resolution
        # order: request.session_settings > writing_session.settings >
        # user default > "numbered". This carries the mission's chosen
        # profile through the "Continue Writing" handoff, which used to
        # drop the value and fall back to numbered [1]/[2] refs.
        from services.citation_profiles import resolve_citation_profile
        user_settings = current_user.settings if current_user.settings else {}
        session_settings_obj = (
            getattr(writing_session, "settings", None) or {}
        )
        session_citation_profile_id = None
        if isinstance(session_settings_obj, dict):
            session_citation_profile_id = session_settings_obj.get("citation_profile_id")
        request_citation_profile_id = None
        if request.session_settings and hasattr(request.session_settings, "model_dump"):
            request_citation_profile_id = (
                request.session_settings.model_dump().get("citation_profile_id")
            )
        effective_profile_id = (
            request_citation_profile_id or session_citation_profile_id
        )
        session_override = (
            {"citation_profile_id": effective_profile_id}
            if effective_profile_id
            else None
        )
        citation_profile = resolve_citation_profile(session_override, user_settings)

        # Session-mode classifier (#49). Honours a persisted
        # `session_mode` on the writing-session settings blob, with
        # a sensible default derived from the session's context:
        # sessions created from a research handoff land on
        # "iterative_revision"; fresh Writing-tab sessions land on
        # "fresh_research". The agent reads this to decide whether to
        # short-circuit the router.
        session_mode = None
        if isinstance(session_settings_obj, dict):
            session_mode = session_settings_obj.get("session_mode")
        if session_mode not in ("fresh_research", "iterative_revision", "mixed"):
            # Fallback: infer from chat metadata — chats created via
            # handoff have a title starting with "Writing: " (see
            # DraftTab). Defaults stay conservative (fresh_research)
            # so we don't accidentally suppress retrieval.
            chat_row = (
                db.query(models.Chat).filter(models.Chat.id == chat_id).first()
            )
            title = (chat_row.title if chat_row else "") or ""
            if title.startswith("Writing:"):
                session_mode = "iterative_revision"
            else:
                session_mode = "fresh_research"

        from services.writing_flags import WritingFlags
        flags = WritingFlags.resolve(user_settings)
        structured_refs_on = flags.structured_bibliography

        context_info = {
            "document_group_id": document_group_id,
            "document_group_ids": effective_group_ids,
            "document_group_name": document_group_name,
            "document_group_names": document_group_names,
            "filter_doc_ids": filter_doc_ids,
            "use_web_search": use_web_search,
            "operation_mode": request.operation_mode or "balanced",
            "session_mode": session_mode,
            "user_profile": {
                "full_name": current_user.full_name,
                "location": current_user.location,
                "job_title": current_user.job_title,
            },
            "session_id": session_id,
            "custom_system_prompt": custom_system_prompt,
            "citation_mode": citation_profile.citation_mode,
            "citation_profile_id": citation_profile.id,
            "user_settings": user_settings,
            "structured_bibliography_enabled": structured_refs_on,
            "search_config": {
                "deep_search": request.deep_search or False,
                "max_iterations": request.max_search_iterations or default_iterations,
                "max_decomposed_queries": request.max_decomposed_queries or default_queries,
                "max_results": get_writing_mode_web_results(),  # Use writing mode web results setting
                "max_doc_results": get_writing_mode_doc_results()  # Use writing mode doc results setting
            }
        }
        
        # Create status callback for agent updates
        async def status_callback(status: str, details: str = ""):
            """Send status updates via WebSocket."""
            try:
                # Map agent status to user-friendly messages
                status_map = {
                    "analyzing": "analyzing",
                    "router_thinking": "thinking",
                    "router_decision": "planning",
                    "searching_web": "searching",
                    "searching_documents": "searching",
                    "assessing_relevance": "evaluating",
                    "fetching_content": "retrieving",
                    "generating_response": "writing",
                    "generating": "writing",
                    "finalizing": "finalizing",
                    "complete": "complete",
                    "error": "error"
                }
                
                mapped_status = status_map.get(status, status)
                await send_agent_status_update(session_id, mapped_status, details)
                
                # Don't stream status messages as content chunks
                logger.debug(f"Status update: {status} - {details}")
            except Exception as e:
                logger.warning(f"Failed to send status update: {e}")
        
        # Don't send initial status - let the agent handle it

        # Deliverable planner pre-pass. When the flag is on, run one
        # cheap LLM call BEFORE the writer to decide section count,
        # word budget, language, and figure intent. Reuse the persisted
        # plan from a prior turn in this session if one exists. The
        # continuation detector consumes the plan as ground truth so a
        # truncated draft can't masquerade as complete.
        deliverable_plan = None
        if flags.deliverable_planner:
            from services.writing_planner import (
                load_plan_from_session,
                plan_deliverable,
                serialise_plan_to_session,
            )
            deliverable_plan = load_plan_from_session(session_settings_obj)
            if deliverable_plan is None:
                try:
                    deliverable_plan = await plan_deliverable(
                        prompt=request.message or "",
                        existing_draft_body=draft.content or "",
                        dispatcher=writing_controller.model_dispatcher,
                    )
                    if deliverable_plan is not None:
                        writing_session.settings = serialise_plan_to_session(
                            session_settings_obj, deliverable_plan
                        )
                        db.commit()
                except Exception as exc:
                    logger.warning(
                        "Deliverable planner failed (non-fatal): %s",
                        exc,
                        exc_info=True,
                    )
            if deliverable_plan is not None:
                context_info["expected_sections"] = deliverable_plan.expected_sections
                context_info["section_budgets"] = deliverable_plan.section_budgets
                # Planner-resolved language is authoritative; user-settings
                # override (set explicitly by the user) still wins.
                if not (user_settings or {}).get("language_code"):
                    context_info.setdefault("user_settings", user_settings or {})
                    if isinstance(context_info.get("user_settings"), dict):
                        context_info["user_settings"]["language_code"] = (
                            deliverable_plan.language_code
                        )

        # Writing Completeness Contract — figure pre-fetch
        # When the prompt or draft carries figure-intent, pre-fetch
        # candidates from document_images and inject them into the
        # agent's context via the custom_system_prompt channel. No-op
        # when intent is absent or the library is empty.
        figure_resolution: Optional[Dict[str, Any]] = None
        try:
            from services.figure_resolution import resolve_figures
            if flags.rag_figures:
                figure_resolution = resolve_figures(
                    db,
                    prompt=request.message or "",
                    draft_body=draft.content or "",
                    doc_ids=filter_doc_ids or [],
                    language_code=(user_settings or {}).get("language_code", "de"),
                )
                if figure_resolution and figure_resolution.get("system_prompt_addendum"):
                    existing = context_info.get("custom_system_prompt") or ""
                    context_info["custom_system_prompt"] = (
                        (existing + "\n\n" if existing else "")
                        + figure_resolution["system_prompt_addendum"]
                    )
                    logger.info(
                        "Writing figure resolver: intent=%s queries=%d candidates_total=%d",
                        figure_resolution.get("intent_detected"),
                        len(figure_resolution.get("queries") or []),
                        sum(
                            len(v)
                            for v in (figure_resolution.get("candidates_by_description") or {}).values()
                        ),
                    )
        except Exception as exc:
            logger.warning(
                "Figure resolver failed (non-fatal): %s", exc, exc_info=True
            )

        # Run the agent with status callback (not streaming callback)
        result = await agent.run(
            prompt=request.message,
            draft_content=draft.content,
            chat_history=chat_history_messages,
            context_info=context_info,
            status_callback=status_callback
        )
        
        # Per-request flag-state telemetry runs here (still has access
        # to user_settings) so the env/user/resolved triple lands in
        # observability. The pipeline below only sees the frozen flags
        # snapshot.
        from services.writing_telemetry import log_flag_state
        log_flag_state(
            subsystem="writing_chat",
            user_settings=user_settings,
            draft_id=draft.id,
            user_id=user_id,
            session_id=session_id,
        )

        # Post-agent processing — bib ingest, audit, citation sync,
        # completeness post-process, persistence, WebSocket payload —
        # all run as one pipeline so the chat task stays a thin caller.
        # Each stage is independently flag-gated inside the pipeline.
        from services.writing_pipeline import (
            PipelineContext,
            run_response_pipeline,
        )
        pipeline_ctx = PipelineContext(
            db=db,
            draft=draft,
            chat_id=chat_id,
            session_id=session_id,
            user_id=user_id,
            task_id=task_id,
            flags=flags,
            citation_profile=citation_profile,
            figure_resolution=figure_resolution,
        )
        pipeline_result = await run_response_pipeline(
            raw_response=result.get("chat_response", "") or "",
            sources=result.get("sources", []),
            context=pipeline_ctx,
        )
        final_response_text = pipeline_result.final_response_text

        # Send the complete response via WebSocket. The WebSocket payload
        # uses the post-processed text so the chat UI shows the same
        # content the DB has.
        await send_agent_status_update(
            session_id, "completed", "Response generated successfully"
        )
        await send_draft_content_update(
            session_id, pipeline_result.websocket_payload, "complete"
        )
        
        # Update chat title if needed
        try:
            title_service = ChatTitleService(writing_controller.model_dispatcher)
            await title_service.update_title_if_needed(
                db=db,
                chat_id=chat_id,
                user_id=user_id,
                user_message=request.message,
                ai_response=result["chat_response"]
            )
        except Exception as e:
            logger.warning(f"Failed to update chat title: {e}")
            
    except asyncio.CancelledError:
        # #45 — user hit the Stop button in the UI. Persist a short
        # terminator message so the chat history is coherent, notify
        # the client, then re-raise so the task's cancelled state is
        # honoured by the asyncio runtime.
        logger.info(f"Writing chat task {task_id} cancelled by user")
        try:
            cancelled_msg = models.Message(
                id=str(uuid.uuid4()),
                chat_id=chat_id,
                role="assistant",
                content="⏹ Anfrage vom Benutzer gestoppt.",
                sources=[],
                created_at=datetime.utcnow(),
            )
            db.add(cancelled_msg)
            db.commit()
        except Exception as persist_err:
            logger.warning(
                f"Failed to persist cancel marker for task {task_id}: {persist_err}"
            )
        try:
            await send_agent_status_update(session_id, "cancelled", "Anfrage gestoppt")
            await send_draft_content_update(
                session_id,
                {
                    "message": "⏹ Anfrage vom Benutzer gestoppt.",
                    "sources": [],
                    "task_id": task_id,
                },
                "cancelled",
            )
        except Exception:
            pass
        raise
    except Exception as e:
        logger.error(f"Error in background writing chat processing: {e}", exc_info=True)
        try:
            await send_streaming_chunk_update(session_id, f"\n❌ Error: {str(e)}\n")
            await send_agent_status_update(session_id, "error", str(e))
        except:
            pass
    finally:
        try:
            db.close()
        finally:
            await _unregister_writing_task(task_id)


@router.delete("/sessions/{session_id}/tasks/{task_id}")
async def cancel_writing_task(
    session_id: str,
    task_id: str,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie),
):
    """Cancel an in-flight writing-chat background task (#45).

    Ownership check: the session_id must belong to a chat owned by the
    current user. Returns ``{\"cancelled\": true}`` when the task was
    running and received the cancel signal, ``{\"cancelled\": false,
    \"reason\": \"not_found\"}`` when it was already finished or never
    existed.
    """
    writing_session = (
        db.query(models.WritingSession)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(
            models.WritingSession.id == session_id,
            models.Chat.user_id == current_user.id,
        )
        .first()
    )
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied",
        )

    cancelled = await _cancel_writing_task(task_id)
    if cancelled:
        logger.info(
            f"User {current_user.id} cancelled writing task {task_id} "
            f"on session {session_id}"
        )
        return {"cancelled": True, "task_id": task_id}
    return {"cancelled": False, "reason": "not_found", "task_id": task_id}


@router.put("/sessions/{session_id}/settings", response_model=schemas.WritingSession)
async def update_session_settings(
    session_id: str,
    settings_update: schemas.WritingSessionSettingsUpdate,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Update writing session settings including revision parameters."""
    
    # Get writing session and verify access
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()
    
    if not writing_session:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail="Writing session not found or access denied"
        )
    
    # Update settings
    current_settings = writing_session.settings or {}
    new_settings = settings_update.settings.dict(exclude_unset=True)
    current_settings.update(new_settings)
    
    writing_session.settings = current_settings
    writing_session.updated_at = datetime.utcnow()
    
    db.commit()
    db.refresh(writing_session)
    
    return writing_session


# Word Document Export Endpoint
from pydantic import BaseModel
import pypandoc
import tempfile
import os
from fastapi.responses import FileResponse, Response

class MarkdownContent(BaseModel):
    markdown_content: str
    filename: Optional[str] = None
    # #57: when draft_id is provided AND the structured-bibliography flag
    # is on AND the draft has structured refs, the export path strips any
    # inline Literaturverzeichnis in markdown_content and appends a fresh
    # render from the registry. Legacy drafts without structured refs
    # export unchanged.
    draft_id: Optional[str] = None
    citation_profile_id: Optional[str] = None


def _strip_inline_section(markdown: str, heading_pattern: str) -> str:
    """Remove an inline section starting at a `## heading_pattern` line.

    Matches the heading line (level 2 only) and drops from there up to
    but not including the next level-1 or level-2 heading — so deeper
    sub-sections (`### …`) inside the target section get stripped along
    with it, but sibling level-2 sections (e.g. `## Anhang`) survive.

    #75 fix: the earlier implementation used `re.DOTALL` + `.*?` + a
    lookahead that only checked for `## `, which meant a
    `## Literaturverzeichnis` with `### Primärliteratur` / `### Sekundärliteratur`
    sub-sections got fully swallowed — correct for THIS use case — but
    followed by an unrelated `### Appendix` at the wrong level, the
    stripper kept eating. The explicit `\\n(?=#{1,2}\\s)|\\Z` bound
    below stops at the next level-1/level-2 heading.
    """
    import re as _re

    full = _re.compile(
        rf"(?:^|\n)##\s+(?:{heading_pattern})\s*\n"
        rf"(?:(?!\n#{{1,2}}\s).)*",
        flags=_re.DOTALL,
    )
    return full.sub("", markdown)


def _strip_inline_bibliography(markdown: str) -> str:
    """Remove inline ## Literaturverzeichnis / ## References section."""
    return _strip_inline_section(markdown, "Literaturverzeichnis|References")


def _strip_inline_portfolio(markdown: str) -> str:
    """Remove inline ## Literaturportfolio / ## Literature Portfolio section (#67)."""
    return _strip_inline_section(markdown, "Literaturportfolio|Literature Portfolio")


def _maybe_append_structured_bibliography(
    db: Session,
    user: models.User,
    draft_id: Optional[str],
    markdown: str,
    profile_id_override: Optional[str] = None,
) -> str:
    """If the draft has structured refs AND the flag is on, replace any
    inline bibliography in `markdown` with the rendered registry."""
    if not draft_id:
        return markdown

    from services.feature_flags import structured_bibliography_enabled
    from services.citation_rendering import render_bibliography
    from services.citation_profiles import resolve_citation_profile

    if not structured_bibliography_enabled(user.settings):
        return markdown

    # Collect structured refs for this draft (owned by this user)
    refs = (
        db.query(models.Reference)
        .join(models.Draft, models.Reference.draft_id == models.Draft.id)
        .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(
            models.Reference.draft_id == draft_id,
            models.Chat.user_id == user.id,
            models.Reference.entry_key.isnot(None),
        )
        .all()
    )
    if not refs:
        return markdown

    entries = [
        {
            "entry_key": r.entry_key,
            "authors": r.authors,
            "year": r.year,
            "title": r.title,
            "container_title": r.container_title,
            "publisher": r.publisher,
            "pages": r.pages,
            "url": r.url or r.web_url,
            "accessed_at": r.accessed_at,
            "doi": r.doi,
            "reference_type": r.reference_type,
        }
        for r in refs
    ]

    profile_id = profile_id_override
    if not profile_id:
        profile = resolve_citation_profile(None, user.settings)
        profile_id = profile.id if profile else "numbered"

    rendered = render_bibliography(entries, profile_id, include_heading=True)
    if not rendered:
        return markdown

    stripped = _strip_inline_bibliography(markdown).rstrip()
    return f"{stripped}\n\n{rendered}"


def _maybe_append_portfolio_section(
    db: Session,
    user: models.User,
    draft_id: Optional[str],
    markdown: str,
) -> str:
    """Append ## Literaturportfolio from drafts.portfolio_output (#67).

    Runs after _maybe_append_structured_bibliography so the order in the
    export is: body → Literaturverzeichnis → Literaturportfolio. Strips
    any inline portfolio section first to avoid duplicates. No-op when
    the column is null or the flag is off.
    """
    if not draft_id:
        return markdown

    from services.feature_flags import structured_bibliography_enabled

    if not structured_bibliography_enabled(user.settings):
        return markdown

    draft = (
        db.query(models.Draft)
        .join(models.WritingSession, models.Draft.writing_session_id == models.WritingSession.id)
        .join(models.Chat, models.WritingSession.chat_id == models.Chat.id)
        .filter(models.Draft.id == draft_id, models.Chat.user_id == user.id)
        .first()
    )
    if draft is None or not draft.portfolio_output:
        return markdown

    portfolio = draft.portfolio_output
    if not isinstance(portfolio, dict):
        return markdown
    rendered = portfolio.get("markdown_table") or ""
    if not rendered.strip():
        return markdown

    stripped = _strip_inline_portfolio(markdown).rstrip()
    return f"{stripped}\n\n{rendered}\n"


@router.post("/sessions/{session_id}/draft/docx")
async def export_draft_as_docx(
    session_id: str,
    content: MarkdownContent,
    db: Session = Depends(get_db),
    current_user: models.User = Depends(get_current_user_from_cookie)
):
    """Export a writing draft as a Word document."""

    # Verify the user has access to this writing session
    writing_session = db.query(models.WritingSession).join(
        models.Chat, models.WritingSession.chat_id == models.Chat.id
    ).filter(
        models.WritingSession.id == session_id,
        models.Chat.user_id == current_user.id
    ).first()

    if not writing_session:
        raise HTTPException(status_code=404, detail="Writing session not found or access denied")

    original_markdown = content.markdown_content
    markdown_to_render = _maybe_append_structured_bibliography(
        db=db,
        user=current_user,
        draft_id=content.draft_id,
        markdown=original_markdown,
        profile_id_override=content.citation_profile_id,
    )
    bib_source = (
        "structured" if markdown_to_render is not original_markdown and markdown_to_render != original_markdown
        else ("inline" if "## Literaturverzeichnis" in original_markdown or "## References" in original_markdown else "none")
    )
    post_bib_markdown = markdown_to_render
    markdown_to_render = _maybe_append_portfolio_section(
        db=db,
        user=current_user,
        draft_id=content.draft_id,
        markdown=markdown_to_render,
    )
    port_source = (
        "structured" if markdown_to_render != post_bib_markdown
        else ("inline" if "## Literaturportfolio" in original_markdown or "## Literature Portfolio" in original_markdown else "none")
    )

    from services.writing_telemetry import record_docx_export
    record_docx_export(
        bibliography_source=bib_source,  # type: ignore[arg-type]
        portfolio_source=port_source,  # type: ignore[arg-type]
        draft_id=content.draft_id,
        user_id=current_user.id,
        markdown_size=len(markdown_to_render),
    )

    try:
        # Create a temporary file for the DOCX output
        with tempfile.NamedTemporaryFile(suffix='.docx', delete=False) as temp_file:
            temp_path = temp_file.name

        try:
            # Convert markdown to DOCX using pypandoc with output file
            pypandoc.convert_text(
                markdown_to_render,
                'docx',
                format='md',
                outputfile=temp_path,
                extra_args=[f'--reference-doc={REFERENCE_DOC_PATH}'] if REFERENCE_DOC_PATH.exists() else []
            )
            
            # Read the generated file
            with open(temp_path, 'rb') as docx_file:
                docx_content = docx_file.read()
            
            # Clean up the temp file
            os.unlink(temp_path)
            
            # Return the file response as bytes
            filename = f"{content.filename or 'document'}.docx"
            return Response(
                content=docx_content,
                media_type='application/vnd.openxmlformats-officedocument.wordprocessingml.document',
                headers={
                    'Content-Disposition': f'attachment; filename="{filename}"'
                }
            )
            
        except Exception as e:
            # Clean up temp file on error
            if os.path.exists(temp_path):
                os.unlink(temp_path)
            raise e
            
    except Exception as e:
        logger.error(f"Failed to convert markdown to DOCX: {str(e)}")
        raise HTTPException(
            status_code=500,
            detail=f"Failed to generate Word document: {str(e)}"
        )
