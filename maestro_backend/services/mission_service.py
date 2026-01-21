"""
Mission Service - Shared functionality for mission operations
"""

import logging
import uuid
from typing import Optional, Dict, Any
from datetime import datetime
from database.database import get_db
from database import crud
from ai_researcher.user_context import get_current_user

logger = logging.getLogger(__name__)


async def prepare_mission_start(
    mission_id: str,
    mission_context: Any,
    context_mgr: Any,
    settings: Dict[str, Any],
    log_to_frontend: bool = True
) -> Dict[str, Any]:
    """
    Unified function to prepare a mission for starting.
    This handles all common logic needed when starting a mission from any source.

    Args:
        mission_id: The mission ID
        mission_context: The mission context object
        context_mgr: The context manager for mission operations
        settings: Dictionary containing:
            - use_web_search: bool
            - document_group_id: Optional[str]
            - auto_create_document_group: bool
            - current_research_params: Optional[dict]
        log_to_frontend: Whether to log events to frontend

    Returns:
        Updated settings dictionary with any newly created resources
    """
    logger.info(f"Preparing mission {mission_id} for start with settings: {settings}")

    # Extract settings
    use_web_search = settings.get("use_web_search", True)
    document_group_id = settings.get("document_group_id")
    auto_create_document_group = settings.get("auto_create_document_group", False)
    current_research_params = settings.get("current_research_params", {})

    # Get current user
    current_user = get_current_user()
    if not current_user:
        raise ValueError("No current user found")

    # Build tool selection
    tool_selection = {
        "web_search": use_web_search,
        "local_rag": document_group_id is not None
    }

    # Get existing metadata
    existing_metadata = mission_context.metadata or {}

    # Create document group if auto_create_document_group is enabled
    # This is INDEPENDENT of whether there's a search group selected!
    # - document_group_id = group to SEARCH from (user-selected)
    # - generated_document_group_id = group to SAVE to (auto-created)
    logger.info(f"Document group creation check - auto_create: {auto_create_document_group}, search_group_id: {document_group_id}")

    # Check if we already have a generated group (don't create duplicates)
    already_has_generated_group = existing_metadata.get("generated_document_group_id")

    if auto_create_document_group and not already_has_generated_group:
        logger.info(f"auto_create_document_group is enabled for mission {mission_id}, creating document group...")

        db = next(get_db())
        try:
            # Get the mission's chat for reference
            mission_db = crud.get_mission(db, mission_id=mission_id, user_id=current_user.id)
            chat_db = None
            if mission_db and mission_db.chat_id:
                chat_db = crud.get_chat(db, chat_id=mission_db.chat_id, user_id=current_user.id)

            # Create a concise name for the document group
            user_request = mission_context.user_request
            request_lines = user_request.split('\n')
            first_line = request_lines[0] if request_lines else user_request

            # Clean up the request text - remove extra spaces and punctuation
            clean_request = first_line.strip()

            # Create a concise name with "R: " prefix (max ~50 chars total)
            max_length = 45  # Leave room for "R: " prefix
            if len(clean_request) > max_length:
                group_name = f"R: {clean_request[:max_length]}..."
            else:
                group_name = f"R: {clean_request}"

            # Create the document group with a new UUID
            group_id = str(uuid.uuid4())
            new_group = crud.create_document_group(
                db=db,
                group_id=group_id,
                user_id=current_user.id,
                name=group_name,
                description=f"Auto-created for research mission: {user_request[:200]}"
            )

            if new_group:
                # IMPORTANT: Auto-save creates a group for SAVING documents only
                # It should NOT enable local_rag for searching!
                # Keep tool_selection as it was - don't change local_rag

                # Update mission metadata with the new document group FOR SAVING ONLY
                existing_metadata.update({
                    # Don't set document_group_id - that's for searching
                    # "document_group_id": new_group.id,  # NO! This enables search
                    # "use_local_rag": True,  # NO! User didn't select a group to search
                    "auto_created_group_id": new_group.id,
                    "generated_document_group_id": new_group.id,  # For saving documents
                    "generated_document_group_name": new_group.name
                })

                # Update the mission in database if it exists
                if mission_db:
                    mission_db.generated_document_group_id = new_group.id
                    db.commit()

                # Don't update chat settings with the auto-created group
                # The user didn't select this group - it's only for saving
                # if chat_db:
                #     chat_settings = chat_db.settings or {}
                #     chat_settings["document_group_id"] = new_group.id  # NO!
                #     crud.update_chat_settings(
                #         db=db,
                #         chat_id=chat_db.id,
                #         settings=chat_settings
                #     )

                logger.info(f"Successfully created document group '{new_group.name}' (ID: {new_group.id}) for mission {mission_id}")

                # Log to frontend if requested
                if log_to_frontend:
                    await context_mgr.log_execution_step(
                        mission_id=mission_id,
                        agent_name="System",
                        action="Document Group Created",
                        output_summary=f"Auto-created document group '{group_name}' for collecting research documents.",
                        status="success"
                    )
        except Exception as e:
            logger.error(f"Failed to create auto document group for mission {mission_id}: {e}")
            # Continue without document group - this shouldn't block the mission
        finally:
            db.close()

    # Build comprehensive_settings
    user_settings = current_user.settings if current_user and hasattr(current_user, 'settings') else {}

    # Extract various settings categories
    ai_settings = user_settings.get("ai_endpoints", {})
    model_config = {
        "fast_provider": ai_settings.get("fast_llm_provider"),
        "fast_model": ai_settings.get("fast_llm_model"),
        "mid_provider": ai_settings.get("mid_llm_provider"),
        "mid_model": ai_settings.get("mid_llm_model"),
        "intelligent_provider": ai_settings.get("intelligent_llm_provider"),
        "intelligent_model": ai_settings.get("intelligent_llm_model"),
        "verifier_provider": ai_settings.get("verifier_llm_provider"),
        "verifier_model": ai_settings.get("verifier_llm_model"),
    }

    search_settings = user_settings.get("search", {})
    web_fetch_settings = user_settings.get("web_fetch", {})

    # Ensure research_params includes auto_create_document_group
    if current_research_params is None:
        current_research_params = {}
    current_research_params["auto_create_document_group"] = auto_create_document_group

    # Build comprehensive settings
    comprehensive_settings = {
        "use_web_search": use_web_search,
        "use_local_rag": document_group_id is not None,
        "auto_create_document_group": auto_create_document_group,
        "document_group_id": document_group_id,
        "document_group_name": existing_metadata.get("document_group_name"),
        "model_config": model_config,
        "research_params": current_research_params,
        "search_provider": search_settings.get("provider"),
        "web_fetch_settings": web_fetch_settings,
        "all_user_settings": user_settings,
        "settings_captured_at": datetime.now().isoformat(),
        "settings_captured_at_start": True,
        "start_time_capture": datetime.now().isoformat()
    }

    # Update mission metadata with all settings
    # IMPORTANT: document_group_id and use_local_rag are for SEARCHING
    # They should only be set if user explicitly selected a group to search
    existing_metadata.update({
        "tool_selection": tool_selection,
        "document_group_id": document_group_id,  # This stays as passed in (None if no search group)
        "use_web_search": use_web_search,
        "use_local_rag": document_group_id is not None,  # Only true if user selected a group to SEARCH
        "auto_create_document_group": auto_create_document_group,
        "research_params": current_research_params,
        "comprehensive_settings": comprehensive_settings,
        "settings_captured_at_start": True,
        "start_time_capture": datetime.now().isoformat()
    })

    # Save the updated metadata
    await context_mgr.update_mission_metadata(mission_id, existing_metadata)

    # Log what we've set for debugging
    logger.info(f"Mission {mission_id} prepared with settings - Web Search: {use_web_search}, Doc Group: {document_group_id}, Auto-save: {auto_create_document_group}")
    logger.info(f"Mission {mission_id} metadata includes generated_document_group_id: {existing_metadata.get('generated_document_group_id')}")
    logger.info(f"Mission {mission_id} research_params: {existing_metadata.get('research_params', {})}")

    # Return updated settings
    # IMPORTANT: Keep document_group_id as the original search group (None if not searching)
    # The auto-created group is only for saving, not searching
    return {
        "use_web_search": use_web_search,
        "document_group_id": document_group_id,  # Original search group (None if no search)
        "auto_create_document_group": auto_create_document_group,
        "tool_selection": tool_selection,
        "comprehensive_settings": comprehensive_settings,
        "metadata": existing_metadata,
        "generated_document_group_id": existing_metadata.get("generated_document_group_id")  # For reference
    }