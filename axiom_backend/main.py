from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
import logging
import os
from concurrent.futures import ThreadPoolExecutor
import asyncio

from database.database import (
    SessionLocal,
    test_connection,
    init_db,
    connect_with_retries,
    engine,
)
from database import crud
from api import auth, missions, system, chat, chats, documents, websockets, settings, writing, dashboard, admin, research_reports, languages, rag, openai_compat
from middleware import user_context_middleware
from config.paths import LEGACY_MARKDOWN_PATH

# Configure reduced logging to minimize console noise
from logging_config import setup_logging
setup_logging()  # Will use LOG_LEVEL environment variable
logger = logging.getLogger(__name__)

app = FastAPI(
    title="Axiom API",
    description="AI Research Assistant API",
    version="2.0.0-alpha"
)

# Configure CORS with environment variables
def get_cors_origins():
    """Get CORS allowed origins from environment variables."""
    # Check if we should allow all origins (development mode with nginx proxy)
    allow_wildcard = os.getenv("ALLOW_CORS_WILDCARD", "false").lower() == "true"
    if allow_wildcard:
        logger.info("CORS: Allowing all origins (wildcard mode)")
        return ["*"]
    
    # Build default origins for backward compatibility
    default_origins = [
        "http://localhost",
        "http://localhost:80",
        "http://localhost:3000",
        "http://localhost:3030",
        "http://localhost:5173",
        "http://localhost:8001",
        "http://127.0.0.1",
        "http://127.0.0.1:80",
        "http://127.0.0.1:3000",
        "http://127.0.0.1:3030",
        "http://127.0.0.1:8001"
    ]
    
    # Get additional origins from environment variable
    cors_origins_env = os.getenv("CORS_ALLOWED_ORIGINS", "")
    if cors_origins_env == "*":
        logger.info("CORS: Allowing all origins via CORS_ALLOWED_ORIGINS=*")
        return ["*"]
    elif cors_origins_env:
        # Split by comma and strip whitespace
        additional_origins = [origin.strip() for origin in cors_origins_env.split(",") if origin.strip()]
        # Combine with defaults, removing duplicates
        all_origins = list(set(default_origins + additional_origins))
        logger.info(f"CORS allowed origins configured: {all_origins}")
        return all_origins
    
    # Also add origins based on old environment variables for backward compatibility
    frontend_host = os.getenv("FRONTEND_HOST")
    backend_host = os.getenv("BACKEND_HOST")
    if frontend_host or backend_host:
        if frontend_host:
            frontend_port = os.getenv("FRONTEND_PORT", "3030")
            default_origins.append(f"http://{frontend_host}:{frontend_port}")
            default_origins.append(f"http://{frontend_host}")
        if backend_host:
            backend_port = os.getenv("BACKEND_PORT", "8001")
            default_origins.append(f"http://{backend_host}:{backend_port}")
            default_origins.append(f"http://{backend_host}")
    
    logger.info(f"CORS allowed origins (defaults): {list(set(default_origins))}")
    return list(set(default_origins))

app.add_middleware(
    CORSMiddleware,
    allow_origins=get_cors_origins(),
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
    expose_headers=["*"],
    # Add max age to reduce preflight requests
    max_age=86400,  # 24 hours
)

# Add user context middleware
app.middleware("http")(user_context_middleware)

# Track user-facing request activity for smart idle unload
from api.activity_middleware import ActivityTrackerMiddleware
app.add_middleware(ActivityTrackerMiddleware)

# Include API routers
app.include_router(auth.router, prefix="/api/auth", tags=["auth"])
app.include_router(missions.router, prefix="/api", tags=["missions"])
app.include_router(system.router, prefix="/api/system", tags=["system"])
app.include_router(chat.router, prefix="/api", tags=["chat"])
app.include_router(chats.router, prefix="/api", tags=["chats"])
app.include_router(documents.router, prefix="/api", tags=["documents"])
app.include_router(settings.router, prefix="/api", tags=["settings"])
app.include_router(dashboard.router, prefix="/api/dashboard", tags=["dashboard"])
app.include_router(writing.router, tags=["writing"])
app.include_router(websockets.router, tags=["websockets"])
app.include_router(admin.router)
app.include_router(research_reports.router, tags=["research_reports"])
app.include_router(languages.router, tags=["languages"])
app.include_router(rag.router, tags=["rag"])
app.include_router(openai_compat.router, prefix="/api", tags=["openai-compat"])

@app.on_event("startup")
async def startup_event():
    """Initialize database, AI components and create first user on startup."""
    # Only log at ERROR level or higher based on LOG_LEVEL setting
    
    # Store the main event loop reference for WebSocket updates from background threads
    from ai_researcher.agentic_layer.context_manager import set_main_event_loop
    set_main_event_loop()
    
    # Initialize database connection and tables
    try:
        # Verify database connectivity with retries instead of a single-shot
        # probe. init_postgres (run by start.sh) already migrated, but a
        # transient macvlan blip in the few seconds between that and uvicorn's
        # startup event would otherwise fail app startup with a false-negative
        # 'Database connection failed' (review finding 3).
        if not connect_with_retries(
            engine, max_retries=10, base_delay=2.0, purpose="app startup"
        ):
            logger.error("Failed to connect to database after retries")
            raise Exception("Database connection failed")
        
        # Initialize database tables
        init_db()
        logger.info("Database initialized successfully")

        # For PostgreSQL, ensure required extensions are available
        if os.getenv("DATABASE_URL", "").startswith("postgresql"):
            from database.init_postgres import ensure_extensions, run_column_migrations
            ensure_extensions()
            column_failures = run_column_migrations()
            if column_failures:
                raise RuntimeError(
                    f"Column migrations failed: {column_failures}"
                )

        # Initialize PromptLoader for multilingual support
        try:
            from ai_researcher.agentic_layer.services.prompt_loader import init_prompt_loader
            db = SessionLocal()
            try:
                init_prompt_loader(db)
                logger.info("PromptLoader initialized successfully")
            finally:
                db.close()
        except Exception as e:
            logger.warning(f"PromptLoader initialization failed: {e}. Will use hardcoded prompts as fallback.", exc_info=True)

    except Exception as e:
        # DB-critical startup failure: re-raise so uvicorn aborts startup and
        # systemd restarts the unit (which then retries the full init). The
        # previous 'continue anyway' let the process reach 'Application startup
        # complete' with a broken DB layer, undermining the fail-fast behavior
        # in start.sh (review finding 1).
        logger.error(f"Database initialization failed: {e}", exc_info=True)
        raise
    
    # Create a configurable thread pool
    # Increased default from 10 to 20 to handle concurrent web fetches better
    max_workers = int(os.getenv("MAX_WORKER_THREADS", "20"))
    app.state.thread_pool = ThreadPoolExecutor(max_workers=max_workers)
    logger.info(f"Initialized thread pool with {max_workers} workers")

    # The backend owns the shared GPU worker subprocess (#9) that
    # doc-processor connects to as a client. Spawn it eagerly here so
    # the socket exists before the first doc-processor import arrives.
    try:
        from ai_researcher.gpu_worker.client import get_client
        client = get_client()
        if not client._client_mode:
            import threading
            def _warmup():
                try:
                    client._ensure_worker()
                    logger.info("GPU worker subprocess spawned at startup")
                except Exception as exc:
                    logger.warning(
                        f"Eager GPU worker spawn failed (will retry lazily): {exc}"
                    )
            threading.Thread(target=_warmup, daemon=True, name="gpu-worker-warmup").start()
    except Exception as exc:
        logger.warning(f"Could not trigger GPU worker warmup: {exc}")

    # Create first user for development if no users exist
    db = SessionLocal()
    try:
        users = crud.get_users(db)
        if not users:
            from setup_first_user import create_first_user
            create_first_user()
    except Exception as e:
        logger.error(f"Error during initial user check: {e}", exc_info=True)
    finally:
        db.close()
    
    # Clean up dangling CLI-ingested documents
    db = SessionLocal()
    try:
        from database.models import Document, document_group_association
        logger.info("Checking for dangling CLI documents...")
        
        # Find and clean up documents with cli_processing status
        cli_documents = db.query(Document).filter(
            Document.processing_status == "cli_processing"
        ).all()
        
        if cli_documents:
            logger.info(f"Found {len(cli_documents)} dangling CLI documents, cleaning up...")
            deleted_count = 0
            
            for doc in cli_documents:
                try:
                    # Delete associated files
                    if doc.file_path and os.path.exists(doc.file_path):
                        os.remove(doc.file_path)
                    
                    markdown_path = str(LEGACY_MARKDOWN_PATH / f"{doc.id}.md")
                    if os.path.exists(markdown_path):
                        os.remove(markdown_path)
                    
                    # Remove from document groups
                    db.execute(
                        document_group_association.delete().where(
                            document_group_association.c.document_id == doc.id
                        )
                    )
                    
                    # Delete document record
                    db.delete(doc)
                    deleted_count += 1
                except Exception as e:
                    logger.warning(f"Failed to clean up document {doc.id}: {e}")
            
            db.commit()
            logger.info(f"Cleaned up {deleted_count} dangling CLI documents")
        else:
            logger.debug("No dangling CLI documents found")
            
    except Exception as e:
        logger.error(f"Failed to run CLI document cleanup: {e}", exc_info=True)
        # Don't fail startup if cleanup fails
    finally:
        db.close()
    
    # Initialize AI research components
    try:
        from api.missions import initialize_ai_components
        success = await initialize_ai_components()
        if not success:
            logger.error("Failed to initialize AI research components")
    except Exception as e:
        logger.error(f"Error during AI component initialization: {e}", exc_info=True)
    
    # Print startup completion message directly to stdout (visible at any log level)
    # Get the external accessible port from environment (what nginx exposes)
    axiom_port = os.getenv("Axiom_PORT", "80")
    
    # Determine the access URL based on configuration
    # This matches what the setup script configures
    if axiom_port == "80":
        access_url = "http://localhost"
    else:
        access_url = f"http://localhost:{axiom_port}"
    
    print("\n" + "="*60)
    print("Axiom Backend Started Successfully!")
    print("="*60)
    print(f"Access Axiom at: {access_url}")
    print(f"API documentation: {access_url}/docs")
    print("Ready to handle requests")
    print("="*60 + "\n", flush=True)

@app.on_event("shutdown")
async def shutdown_event():
    """Clean up resources on shutdown."""
    # Only log at ERROR level or higher based on LOG_LEVEL setting
    if hasattr(app.state, "thread_pool"):
        app.state.thread_pool.shutdown(wait=True)
    
    # No need to stop monitoring since we only run once at startup
    pass

@app.get("/")
def read_root():
    return {
        "message": "Axiom API v2.0",
        "status": "running",
        "docs": "/docs"
    }

@app.get("/health")
def health_check():
    return {"status": "healthy"}
