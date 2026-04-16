"""
Background document processor service for handling asynchronous document processing
with real-time progress updates via WebSocket.

This service implements a queue-based system to process documents one at a time
to avoid GPU VRAM conflicts while keeping the processing non-blocking.
"""
import asyncio
import os
import uuid
import json
import traceback
from threading import Thread, RLock, Event
from pathlib import Path
from typing import Dict, Any, Optional, Callable, List
from datetime import datetime
from sqlalchemy.orm import Session
import time
from dataclasses import dataclass

# Idle unload settings (opt-in via AXIOM_AUTO_UNLOAD=true)
AUTO_UNLOAD_ENABLED = os.getenv("AXIOM_AUTO_UNLOAD", "false").lower() == "true"
IDLE_UNLOAD_THRESHOLD_SEC = int(os.getenv("AXIOM_IDLE_UNLOAD_SEC", "900"))

from ai_researcher.core_rag.processor import DocumentProcessor
from ai_researcher.core_rag.vector_store_singleton import get_vector_store
from ai_researcher.core_rag.pgvector_store import PGVectorStore as VectorStore  # For type hints
from ai_researcher.core_rag.embedder import TextEmbedder
try:
    from ai_researcher.core_rag.unified_database import UnifiedDocumentDatabase as Database
except ImportError:
    from ai_researcher.core_rag.database import Database
from database import crud, models
from database.database import get_db

@dataclass
class ProcessingJob:
    """Represents a document processing job."""
    job_id: str
    doc_id: str
    user_id: int
    file_path: Path
    original_filename: str
    created_at: datetime

class BackgroundDocumentProcessor:
    """Service for processing documents in the background with progress tracking."""
    
    def __init__(self):
        # Paths to existing document infrastructure
        from config.paths import DATA_BASE_PATH, VECTOR_STORE_PATH, RAW_FILES_PATH, MARKDOWN_PATH, METADATA_PATH, METADATA_DB_PATH
        self.vector_store_path = VECTOR_STORE_PATH
        self.pdf_dir = RAW_FILES_PATH
        self.markdown_dir = MARKDOWN_PATH
        self.metadata_dir = METADATA_PATH
        self.db_path = METADATA_DB_PATH
        
        # Initialize components lazily (only when needed)
        self._vector_store = None
        self._embedder = None
        self._ai_db = None
        self._processor = None
        self._components_lock = RLock()
        
        self.is_processing = False
        self.current_job: Optional[ProcessingJob] = None
        self.shutdown_event = Event()

        # Track last activity for idle unload
        self._last_job_finished_at: float = 0.0
        self._models_loaded: bool = False

        # WebSocket connections for progress updates
        self.websocket_connections: Dict[str, List] = {}

    def start(self):
        """Start the background worker thread."""
        print("Document processing worker started")
        self._worker_loop()

    def _worker_loop(self):
        """Main worker loop that polls the database for queued documents."""
        while not self.shutdown_event.is_set():
            job_processed = False
            db = next(get_db())
            try:
                # Fetch the next queued document
                document = crud.get_next_queued_document(db)
                
                if document:
                    job_processed = True
                    self.is_processing = True
                    
                    # Create a job object
                    job = ProcessingJob(
                        job_id=str(uuid.uuid4()), # This can be improved to use a job ID from the DB
                        doc_id=document.id,
                        user_id=document.user_id,
                        file_path=Path(document.file_path),
                        original_filename=document.original_filename or document.filename,  # Use original_filename for file type detection
                        created_at=document.created_at
                    )
                    self.current_job = job
                    
                    print(f"[{job.doc_id}] Found queued document. Starting processing.")
                    
                    # Mark as processing
                    crud.update_document_status(db, document.id, document.user_id, "processing", 0)
                    
                    # Process the document
                    success = self._process_document_sync(job)
                    
                    # Mark as completed or failed, with cleanup if failed
                    final_status = "completed" if success else "failed"
                    crud.update_document_status(db, document.id, document.user_id, final_status, 100)
                    
                    # If processing failed, clean up any orphaned entries
                    if not success:
                        print(f"[{job.doc_id}] Processing failed, performing cleanup...")
                        try:
                            from database.crud_documents_improved import cleanup_failed_document_improved
                            cleanup_success = cleanup_failed_document_improved(db, document.id, document.user_id)
                            if cleanup_success:
                                print(f"[{job.doc_id}] Successfully cleaned up failed processing artifacts")
                            else:
                                print(f"[{job.doc_id}] Warning: Cleanup encountered some issues")
                        except Exception as cleanup_error:
                            print(f"[{job.doc_id}] Error during cleanup: {cleanup_error}")
                    
                    print(f"[{job.doc_id}] Document processing finished with status: {final_status}")
                    
                    self.is_processing = False
                    self.current_job = None

            except Exception as e:
                print(f"Error in worker loop: {e}")
                traceback.print_exc()
                self.is_processing = False
                if self.current_job:
                    crud.update_document_status(db, self.current_job.doc_id, self.current_job.user_id, "failed", 0)
                    # Also attempt cleanup for the failed job
                    try:
                        from database.crud_documents_improved import cleanup_failed_document_improved
                        cleanup_failed_document_improved(db, self.current_job.doc_id, self.current_job.user_id)
                        print(f"[{self.current_job.doc_id}] Cleaned up after unexpected error")
                    except Exception as cleanup_error:
                        print(f"[{self.current_job.doc_id}] Cleanup after error failed: {cleanup_error}")
                self.current_job = None
            finally:
                db.close()

            # If no job was processed, check for idle unload then wait
            if not job_processed:
                self._maybe_unload_idle_models()
                time.sleep(5)  # Poll every 5 seconds
            else:
                self._last_job_finished_at = time.time()
                self._models_loaded = True
    
    
    def _maybe_unload_idle_models(self):
        """Exit the process if idle long enough. Docker/systemd restarts clean.

        In-process unload is unreliable — PyTorch's CUDA context gets into a bad
        state after `del model + empty_cache()`, causing 'CUDA devices busy' on
        the next load. Exiting the process is the only way to truly free RAM
        and guarantee a clean CUDA context on the next import.
        """
        if not AUTO_UNLOAD_ENABLED or not self._models_loaded:
            return
        if self._last_job_finished_at == 0:
            return
        idle_sec = time.time() - self._last_job_finished_at
        if idle_sec < IDLE_UNLOAD_THRESHOLD_SEC:
            return

        # Belt and suspenders: check for pending docs
        try:
            from sqlalchemy import text as sql_text
            db = next(get_db())
            try:
                pending = db.execute(sql_text(
                    "SELECT count(*) FROM documents WHERE processing_status IN ('pending','processing')"
                )).scalar()
                if pending and pending > 0:
                    return
            finally:
                db.close()
        except Exception:
            pass

        print(
            f"[doc-processor] Auto-unload: {idle_sec:.0f}s idle — exiting process "
            f"for clean RAM + CUDA reset. Container will auto-restart."
        )
        # Flush stdout so the log line is visible before exit
        import sys
        sys.stdout.flush()
        sys.exit(0)

    def _get_vector_store(self) -> VectorStore:
        """Get or initialize the vector store (thread-safe)."""
        with self._components_lock:
            if self._vector_store is None:
                print("Initializing VectorStore singleton...")
                self._vector_store = get_vector_store()
            return self._vector_store
    
    def _get_embedder(self):
        """Get or initialize the embedder (thread-safe).

        In worker mode (``AXIOM_USE_GPU_WORKER=true``) the doc-processor does
        NOT load BGE-M3 itself — it returns an ``EmbedderFacade`` that
        connects to the backend container's GPU worker over a shared Unix
        socket (see issue #9). That way only one BGE-M3 is in VRAM across
        both containers, and idle unload is driven by the single worker.
        """
        with self._components_lock:
            if self._embedder is None:
                if os.getenv("AXIOM_USE_GPU_WORKER", "false").lower() == "true":
                    from ai_researcher.core_rag.gpu_worker_facades import EmbedderFacade
                    print("Doc-processor embedder routed through shared GPU worker")
                    self._embedder = EmbedderFacade()
                else:
                    print("Initializing TextEmbedder...")
                    self._embedder = TextEmbedder(model_name="BAAI/bge-m3")
            return self._embedder
    
    def _get_ai_db(self) -> Database:
        """Get or initialize the AI researcher database (thread-safe)."""
        with self._components_lock:
            if self._ai_db is None:
                print("Initializing AI Database...")
                self._ai_db = Database(db_path=self.db_path)
            return self._ai_db
    
    def _get_processor(self) -> DocumentProcessor:
        """Get or initialize the document processor (thread-safe)."""
        with self._components_lock:
            if self._processor is None:
                print("Initializing DocumentProcessor...")
                embedder = self._get_embedder()
                vector_store = self._get_vector_store()
                
                self._processor = DocumentProcessor(
                    pdf_dir=self.pdf_dir,
                    markdown_dir=self.markdown_dir,
                    metadata_dir=self.metadata_dir,
                    db_path=self.db_path,
                    embedder=embedder,
                    vector_store=vector_store,
                    force_reembed=False
                )
            return self._processor
    
    def _get_processor_with_user_settings(self, user_settings: Dict[str, Any]) -> DocumentProcessor:
        """Get or initialize the document processor with user-specific settings (thread-safe)."""
        with self._components_lock:
            # Reuse existing processor if available, just update the metadata extractor
            if self._processor is None:
                print("Initializing DocumentProcessor with user settings...")
                embedder = self._get_embedder()
                vector_store = self._get_vector_store()
                
                self._processor = DocumentProcessor(
                    pdf_dir=self.pdf_dir,
                    markdown_dir=self.markdown_dir,
                    metadata_dir=self.metadata_dir,
                    db_path=self.db_path,
                    embedder=embedder,
                    vector_store=vector_store,
                    force_reembed=False
                )
            else:
                print("Reusing existing DocumentProcessor, updating metadata extractor...")
            
            # Always update the metadata extractor with current user settings
            from ai_researcher.core_rag.metadata_extractor import MetadataExtractor
            metadata_extractor = MetadataExtractor.from_user_settings(user_settings)
            self._processor.metadata_extractor = metadata_extractor
            
            return self._processor
    
    def add_websocket_connection(self, user_id: str, websocket):
        """Add a WebSocket connection for a user."""
        if user_id not in self.websocket_connections:
            self.websocket_connections[user_id] = []
        self.websocket_connections[user_id].append(websocket)
    
    def remove_websocket_connection(self, user_id: str, websocket):
        """Remove a WebSocket connection for a user."""
        if user_id in self.websocket_connections:
            try:
                self.websocket_connections[user_id].remove(websocket)
                if not self.websocket_connections[user_id]:
                    del self.websocket_connections[user_id]
            except ValueError:
                pass  # Connection not in list
    
    def _send_progress_update_sync(self, user_id: str, update: Dict[str, Any]):
        """Sends a progress update to the main backend via an internal API call."""
        import requests

        # Use environment-aware hostname. With host networking (podman/nerdctl
        # on LXC), localhost works. For Docker with bridge network, set
        # BACKEND_HOST=axiom-backend in .env.
        import os
        backend_host = os.getenv("BACKEND_HOST", "127.0.0.1")
        backend_port = os.getenv("BACKEND_PORT", "8000")
        backend_url = f"http://{backend_host}:{backend_port}/api/internal/document-progress"
        
        try:
            # Add user_id to the update payload if it's not already there
            if 'user_id' not in update:
                update['user_id'] = int(user_id)
            
            response = requests.post(backend_url, json=update, timeout=5)
            response.raise_for_status()
            print(f"Successfully sent progress update to backend for user {user_id}")
        except requests.exceptions.RequestException as e:
            print(f"Error sending progress update to backend: {e}")
    
    def _update_document_progress_sync(self, doc_id: str, user_id: int, progress: int, 
                                     status: str, error_message: Optional[str] = None):
        """Update document progress in database and send WebSocket update (synchronous)."""
        # Get database session
        db = next(get_db())
        try:
            # Update document in database
            document = crud.get_document(db, doc_id=doc_id, user_id=user_id)
            if document:
                document.upload_progress = progress
                document.processing_status = status
                if error_message:
                    document.processing_error = error_message
                db.commit()
            
            # Send WebSocket update
            update = {
                "type": "document_progress",
                "doc_id": doc_id,
                "progress": progress,
                "status": status,
                "error": error_message,
                "timestamp": datetime.utcnow().isoformat()
            }
            self._send_progress_update_sync(str(user_id), update)
            
        except Exception as e:
            print(f"Error updating document progress: {e}")
        finally:
            db.close()
    
    def _update_job_progress_sync(self, job_id: str, user_id: int, progress: int, 
                                status: str, error_message: Optional[str] = None):
        """Update processing job progress in database and send WebSocket update (synchronous)."""
        # Get database session
        db = next(get_db())
        try:
            # Update job in database
            job = db.query(models.DocumentProcessingJob).filter(
                models.DocumentProcessingJob.id == job_id,
                models.DocumentProcessingJob.user_id == user_id
            ).first()
            
            if job:
                job.progress = progress
                job.status = status
                if error_message:
                    job.error_message = error_message
                if status == "running" and not job.started_at:
                    job.started_at = datetime.utcnow()
                elif status in ["completed", "failed"]:
                    job.completed_at = datetime.utcnow()
                db.commit()
            
            # Send WebSocket update
            update = {
                "type": "job_progress",
                "job_id": job_id,
                "document_id": job.document_id if job else None,
                "progress": progress,
                "status": status,
                "error_message": error_message,
                "timestamp": datetime.utcnow().isoformat()
            }
            # self._send_progress_update_sync(str(user_id), update) # Websockets not used in this service
            
        except Exception as e:
            print(f"Error updating job progress: {e}")
        finally:
            db.close()
    
    def _process_document_sync(self, job: ProcessingJob) -> bool:
        """Process a document synchronously in the worker thread."""
        doc_id = job.doc_id
        user_id = job.user_id
        file_path = job.file_path
        original_filename = job.original_filename
        job_id = job.job_id
        
        try:
            # Update status to running
            self._update_job_progress_sync(job_id, user_id, 0, "running")
            self._update_document_progress_sync(doc_id, user_id, 0, "processing")
            
            # Step 1: Get user settings and initialize processor (10% progress)
            print(f"[{doc_id}] Getting user settings and initializing document processor...")
            self._update_job_progress_sync(job_id, user_id, 10, "running")
            
            # Get user settings from database and check for reprocess/re-embed flags
            db = next(get_db())
            try:
                from database import crud
                user = crud.get_user(db, user_id)
                user_settings = user.settings if user and user.settings else {}
                print(f"[{doc_id}] Retrieved user settings for user {user_id}")
                
                # Check for reprocess/re-embed flags in document metadata
                document = crud.get_document(db, doc_id=doc_id, user_id=user_id)
                is_reprocess_only = document and document.metadata_ and document.metadata_.get('reprocess_metadata', False)
                is_reembed = document and document.metadata_ and document.metadata_.get('reembed', False)
                
                # Clear the flags after reading them
                if document and document.metadata_ and ('reprocess_metadata' in document.metadata_ or 'reembed' in document.metadata_):
                    if 'reprocess_metadata' in document.metadata_:
                        del document.metadata_['reprocess_metadata']
                    if 'reembed' in document.metadata_:
                        del document.metadata_['reembed']
                    db.commit()
                
                if is_reprocess_only:
                    print(f"[{doc_id}] REPROCESS MODE: Will only extract metadata, skipping embeddings")
                elif is_reembed:
                    print(f"[{doc_id}] RE-EMBED MODE: Full reprocessing with new embeddings")
                    
            except Exception as e:
                print(f"[{doc_id}] Warning: Could not retrieve user settings: {e}")
                user_settings = {}
                is_reprocess_only = False
                is_reembed = False
            finally:
                db.close()
            
            processor = self._get_processor_with_user_settings(user_settings)

            # Clean old knowledge graph data for this document (handles reprocess case)
            try:
                from sqlalchemy import text as sql_text
                db_clean = next(get_db())
                try:
                    db_clean.execute(sql_text("""
                        DELETE FROM entity_relationships
                        WHERE source_entity_id IN (
                            SELECT entity_id FROM entity_chunk_occurrences WHERE doc_id = CAST(:did AS uuid)
                        ) OR target_entity_id IN (
                            SELECT entity_id FROM entity_chunk_occurrences WHERE doc_id = CAST(:did AS uuid)
                        )
                    """), {"did": doc_id})
                    db_clean.execute(sql_text("""
                        DELETE FROM document_entities
                        WHERE id IN (
                            SELECT entity_id FROM entity_chunk_occurrences WHERE doc_id = CAST(:did AS uuid)
                        )
                    """), {"did": doc_id})
                    db_clean.commit()
                    print(f"[{doc_id}] Cleaned old knowledge graph data")
                finally:
                    db_clean.close()
            except Exception as e:
                print(f"[{doc_id}] Knowledge graph cleanup failed (non-fatal): {e}")

            # Step 2: Process document to Markdown (30% progress)
            file_extension = original_filename.lower().split('.')[-1]
            print(f"[{doc_id}] Starting {file_extension.upper()} processing...")
            self._update_job_progress_sync(job_id, user_id, 30, "running")
            self._update_document_progress_sync(doc_id, user_id, 30, "processing")
            
            # Copy the uploaded file to the expected location with the correct name
            # For backwards compatibility, PDFs go to pdf_dir, others to subdirs
            if original_filename.lower().endswith('.pdf'):
                target_path = self.pdf_dir / f"{doc_id}_{original_filename}"
            elif original_filename.lower().endswith(('.docx', '.doc')):
                word_dir = self.pdf_dir / 'word_documents'
                word_dir.mkdir(parents=True, exist_ok=True)
                target_path = word_dir / f"{doc_id}_{original_filename}"
            elif original_filename.lower().endswith(('.md', '.markdown')):
                markdown_dir = self.pdf_dir / 'markdown_files'
                markdown_dir.mkdir(parents=True, exist_ok=True)
                target_path = markdown_dir / f"{doc_id}_{original_filename}"
            else:
                raise Exception(f"Unsupported file format: {original_filename}")
                
            if not target_path.exists():
                import shutil
                shutil.copy2(file_path, target_path)
                print(f"[{doc_id}] Copied file to processor directory: {target_path}")
            
            # Step 3: Extract metadata and convert to Markdown
            print(f"[{doc_id}] Extracting metadata and converting to Markdown...")
            self._update_document_progress_sync(doc_id, user_id, 35, "processing")
            
            # Extract metadata using appropriate method based on file type
            if original_filename.lower().endswith('.pdf'):
                initial_text = processor._extract_header_footer_text(target_path)
            else:
                initial_text = processor.document_converter.extract_initial_text_for_metadata(target_path)

            # Skip LLM extraction if initial text is too short (image-only cover pages)
            self._update_document_progress_sync(doc_id, user_id, 40, "processing")
            extracted_metadata = None
            if initial_text and len(initial_text.strip()) > 100:
                extracted_metadata = processor.metadata_extractor.extract_and_enrich_sync(initial_text, filename=original_filename)
            else:
                print(f"[{doc_id}] Initial text too short ({len((initial_text or '').strip())} chars), skipping to markdown retry")

            if extracted_metadata:
                final_metadata = {"doc_id": doc_id, "original_filename": original_filename}
                final_metadata.update(extracted_metadata)
            else:
                final_metadata = {"doc_id": doc_id, "original_filename": original_filename}
            
            # Convert document to Markdown based on file type
            self._update_document_progress_sync(doc_id, user_id, 45, "processing")
            marker_images = {}  # Initialize empty dict for non-PDF files
            if original_filename.lower().endswith('.pdf'):
                print(f"[{doc_id}] Converting PDF to Markdown using Marker with intelligent table handling...")
                markdown_content, marker_images = processor._convert_pdf_with_table_handling(target_path)
            elif original_filename.lower().endswith(('.docx', '.doc')):
                print(f"[{doc_id}] Converting Word document to Markdown...")
                markdown_content = processor.document_converter.convert_word_to_markdown(target_path)
            elif original_filename.lower().endswith(('.md', '.markdown')):
                print(f"[{doc_id}] Reading Markdown file content...")
                markdown_content = processor.document_converter.read_markdown_file(target_path)
            else:
                raise Exception(f"Unsupported file format for processing: {original_filename}")

            if not markdown_content:
                raise Exception(f"Document processing produced empty markdown content for {original_filename}")

            # Handle extracted images from Marker
            from ai_researcher import config
            extracted_images = []  # Track extracted images for later embedding
            if config.ENABLE_IMAGE_EXTRACTION and marker_images:
                try:
                    print(f"[{doc_id}] Processing extracted images...")
                    image_dir = processor.image_dir / doc_id
                    image_dir.mkdir(parents=True, exist_ok=True)

                    # Save images from marker output
                    extracted_images = processor._save_marker_images(doc_id, marker_images, image_dir)

                    if extracted_images:
                        # Update markdown references
                        markdown_content = processor._update_markdown_image_paths(markdown_content, doc_id, extracted_images)
                        print(f"[{doc_id}] Organized {len(extracted_images)} images")
                except Exception as e:
                    print(f"[{doc_id}] Warning: Image processing failed: {e}")
                    # Non-fatal error, continue with document processing

            # Save markdown with our doc_id (with updated image paths if applicable)
            md_filename = f"{doc_id}.md"
            md_save_path = processor.markdown_dir / md_filename
            with open(md_save_path, "w", encoding="utf-8") as f:
                f.write(markdown_content)
            print(f"[{doc_id}] Saved Markdown to: {md_save_path}")
            self._update_document_progress_sync(doc_id, user_id, 55, "processing")

            # --- Unload Marker models before embedding (frees ~2.5GB VRAM) ---
            # Peak VRAM during embedding was 11.5GB on 12GB GPU; keeping Marker
            # resident here was the main reason. Marker is no longer needed.
            if original_filename.lower().endswith('.pdf'):
                try:
                    print(f"[{doc_id}] Unloading Marker models (no longer needed)...")
                    for attr in ('table_converter', 'no_table_converter', 'converter', 'marker_models', 'model_dict'):
                        obj = getattr(processor, attr, None)
                        if obj is not None:
                            try:
                                del obj
                            except Exception:
                                pass
                            setattr(processor, attr, None)
                    import gc
                    gc.collect()
                    try:
                        import torch
                        if torch.cuda.is_available():
                            torch.cuda.empty_cache()
                            print(f"[{doc_id}] GPU after Marker unload: {torch.cuda.memory_allocated()/1e9:.1f}GB")
                    except Exception:
                        pass
                except Exception as e_unload:
                    print(f"[{doc_id}] Warning: Marker unload failed: {e_unload}")

            # Retry metadata extraction with markdown content if initial extraction failed
            from services.metadata_enrichment import needs_metadata_retry
            if needs_metadata_retry(extracted_metadata, original_filename):
                from services.metadata_enrichment import prepare_text_sample
                md_sample = prepare_text_sample(markdown_content)
                if md_sample.strip():
                    print(f"[{doc_id}] Retrying metadata extraction with markdown content ({len(md_sample)} chars)...")
                    retry_metadata = processor.metadata_extractor.extract_and_enrich_sync(md_sample, filename=original_filename)
                    if retry_metadata and (retry_metadata.get('title') or retry_metadata.get('authors')):
                        print(f"[{doc_id}] Markdown retry succeeded: title={retry_metadata.get('title', '?')[:50]}")
                        final_metadata = {"doc_id": doc_id, "original_filename": original_filename}
                        final_metadata.update(retry_metadata)
                    else:
                        print(f"[{doc_id}] Markdown retry also failed to extract metadata")

            # Save metadata with our doc_id
            metadata_filename = f"{doc_id}.json"
            metadata_save_path = processor.metadata_dir / metadata_filename
            with open(metadata_save_path, "w", encoding="utf-8") as f:
                import json
                json.dump(final_metadata, f, indent=2, ensure_ascii=False)
            print(f"[{doc_id}] Saved metadata to: {metadata_save_path}")
            self._update_document_progress_sync(doc_id, user_id, 65, "processing")

            # No separate AI database anymore - everything is in the main database
            # The metadata was already saved to JSON file above for reference

            chunks_added_count = 0

            # Skip embeddings if this is metadata-only reprocessing
            if is_reprocess_only:
                print(f"[{doc_id}] SKIPPING embeddings (metadata-only reprocess mode)")
                self._update_document_progress_sync(doc_id, user_id, 90, "processing")
                
                # Get existing chunk count from database
                db_temp = next(get_db())
                try:
                    existing_doc = crud.get_document(db_temp, doc_id=doc_id, user_id=user_id)
                    if existing_doc and hasattr(existing_doc, 'chunk_count'):
                        chunks_added_count = existing_doc.chunk_count or 0
                        print(f"[{doc_id}] Preserving existing chunk count: {chunks_added_count}")
                finally:
                    db_temp.close()
            else:
                # Step 4: Generate embeddings (70% progress)
                print(f"[{doc_id}] Generating embeddings...")
                self._update_job_progress_sync(job_id, user_id, 70, "running")
                self._update_document_progress_sync(doc_id, user_id, 70, "processing")
                
                # Extract page labels for accurate citations
                if original_filename.lower().endswith('.pdf'):
                    try:
                        from ai_researcher.core_rag.processor import extract_page_labels
                        page_labels = extract_page_labels(str(target_path))
                        final_metadata["page_label_map"] = page_labels
                    except Exception as e_pages:
                        print(f"[{doc_id}] Page label extraction failed (non-fatal): {e_pages}")

                # Chunk the content
                print(f"[{doc_id}] Chunking Markdown content...")
                self._update_document_progress_sync(doc_id, user_id, 72, "processing")
                chunks = processor.chunker.chunk(markdown_content, doc_metadata=final_metadata)
                print(f"[{doc_id}] Generated {len(chunks)} chunks")
                self._update_document_progress_sync(doc_id, user_id, 75, "processing")

                # Step 5: Embed and store in vector database
                print(f"[{doc_id}] Embedding and storing in vector database...")
                self._update_document_progress_sync(doc_id, user_id, 78, "processing")
                
                # Embed and store chunks
                if processor.embedder and processor.vector_store and chunks:
                    print(f"[{doc_id}] Embedding {len(chunks)} chunks...")
                    chunks_with_embeddings = processor.embedder.embed_chunks(chunks)
                    
                    # Extract embeddings from chunks for vector store
                    dense_embeddings = [chunk["embeddings"]["dense"] for chunk in chunks_with_embeddings]
                    sparse_embeddings = [chunk["embeddings"]["sparse"] for chunk in chunks_with_embeddings]
                    
                    print(f"[{doc_id}] Adding chunks to vector store in batches...")
                    processor.vector_store.add_chunks(
                        doc_id=doc_id,
                        chunks=chunks_with_embeddings,
                        dense_embeddings=dense_embeddings,
                        sparse_embeddings=sparse_embeddings,
                        batch_size=50  # Process 50 chunks at a time for better performance
                    )
                    chunks_added_count = len(chunks)
                    print(f"[{doc_id}] Successfully added {chunks_added_count} chunks to vector store")
                    self._update_document_progress_sync(doc_id, user_id, 92, "processing")

                    # --- Index in OpenSearch for fulltext search ---
                    if config.ENABLE_OPENSEARCH:
                        try:
                            from ai_researcher.core_rag.opensearch_store import get_opensearch_store
                            os_store = get_opensearch_store()
                            if os_store:
                                os_indexed = os_store.add_chunks(doc_id, chunks_with_embeddings)
                                print(f"[{doc_id}] Indexed {os_indexed} chunks in OpenSearch for fulltext search")
                        except Exception as e_opensearch:
                            print(f"[{doc_id}] Warning: OpenSearch indexing failed: {e_opensearch}")

                    self._update_document_progress_sync(doc_id, user_id, 94, "processing")

                    # Unload embedder after embedding is done -- no longer needed
                    try:
                        from ai_researcher.core_rag.model_cache import model_cache as _mc
                        _mc.unload_embedder()
                        print(f"[{doc_id}] Embedder unloaded after chunk storage")
                    except Exception:
                        pass

                    # --- Build Knowledge Graph ---
                    if config.ENABLE_KNOWLEDGE_GRAPH:
                        try:
                            print(f"[{doc_id}] Building knowledge graph...")
                            from ai_researcher.core_rag.graph_store import GraphStore
                            graph_store = GraphStore()

                            # Build sequential relationships
                            graph_store.build_sequential_relationships(doc_id, len(chunks))
                            print(f"[{doc_id}] Built sequential relationships for {len(chunks)} chunks")

                            # Extract entities (always runs: spaCy for free, LLM when enabled)
                            try:
                                from ai_researcher.core_rag.entity_extractor import EntityExtractor

                                # Detect language once from full markdown
                                doc_language = EntityExtractor.detect_language(markdown_content)

                                entity_extractor = EntityExtractor(language=doc_language)

                                entities_count = 0
                                for chunk in chunks:
                                    entities, _ = entity_extractor.extract_from_chunk_sync(
                                        chunk['text'],
                                        chunk['metadata']
                                    )
                                    for entity in entities:
                                        entity_id = graph_store.add_entity(
                                            entity['text'],
                                            entity['type'],
                                            entity['canonical_form'],
                                            description=entity.get('context_snippet')
                                        )
                                        graph_store.link_entity_to_chunk(
                                            entity_id,
                                            chunk['metadata']['chunk_id'],
                                            doc_id
                                        )
                                        entities_count += 1

                                print(f"[{doc_id}] Extracted {entities_count} entities ({doc_language})")

                                # Unload GLiNER after entity extraction
                                from ai_researcher.core_rag.model_cache import model_cache as _mc2
                                _mc2.unload_gliner()
                                print(f"[{doc_id}] GLiNER unloaded after entity extraction")
                            except Exception as e_entity:
                                print(f"[{doc_id}] Warning: Entity extraction failed: {e_entity}")

                            # Build co-occurrence relationships
                            cooccurrence_count = graph_store.build_cooccurrence_relationships(
                                doc_id=doc_id,
                                min_cooccurrence=2
                            )
                            print(f"[{doc_id}] Built {cooccurrence_count} co-occurrence relationships")

                            # --- mREBEL relation extraction ---
                            try:
                                from ai_researcher.core_rag.relation_extractor import (
                                    extract_relations_from_chunks, unload_mrebel
                                )
                                from ai_researcher.core_rag.model_cache import model_cache

                                # Unload ALL other GPU models before loading mREBEL
                                print(f"[{doc_id}] Unloading GPU models for mREBEL...")
                                model_cache.unload_all()
                                # Clear Marker models from processor
                                if hasattr(processor, 'converter') and processor.converter is not None:
                                    del processor.converter
                                    processor.converter = None
                                if hasattr(processor, 'model_dict') and processor.model_dict is not None:
                                    del processor.model_dict
                                    processor.model_dict = None
                                import torch
                                print(f"[{doc_id}] GPU before mREBEL: {torch.cuda.memory_allocated()/1e9:.1f}GB")

                                print(f"[{doc_id}] Extracting relations with mREBEL...")
                                triples = extract_relations_from_chunks(chunks)

                                # Store triples as entity relationships
                                rel_count = 0
                                for triple in triples:
                                    try:
                                        head_canonical = triple["head"].lower().strip()
                                        tail_canonical = triple["tail"].lower().strip()

                                        # Skip self-referencing triples
                                        if head_canonical == tail_canonical:
                                            continue

                                        head_id = graph_store.add_entity(
                                            triple["head"],
                                            triple["head_type"],
                                            head_canonical,
                                        )
                                        tail_id = graph_store.add_entity(
                                            triple["tail"],
                                            triple["tail_type"],
                                            tail_canonical,
                                        )
                                        graph_store.add_entity_relationship(
                                            source_entity_id=head_id,
                                            target_entity_id=tail_id,
                                            relationship_type=triple["relation"],
                                            strength=0.8,
                                            evidence_chunks=[triple.get("chunk_id", "")],
                                            source="mrebel",
                                        )
                                        rel_count += 1
                                    except Exception as e_rel:
                                        print(f"[{doc_id}] Failed to store triple: {e_rel}")

                                print(f"[{doc_id}] Stored {rel_count} mREBEL relations")

                                # Unload mREBEL to free GPU for future embedder/reranker use
                                unload_mrebel()

                            except Exception as e_mrebel:
                                print(f"[{doc_id}] Warning: mREBEL relation extraction failed: {e_mrebel}")
                                try:
                                    from ai_researcher.core_rag.relation_extractor import unload_mrebel
                                    unload_mrebel()
                                except Exception:
                                    pass

                        except Exception as e_graph:
                            print(f"[{doc_id}] Warning: Knowledge graph building failed: {e_graph}")

                    # NEW: Embed and store images if they were extracted
                    if config.ENABLE_IMAGE_EMBEDDINGS and extracted_images:
                        try:
                            print(f"[{doc_id}] Processing images for embedding...")
                            image_dir = processor.image_dir / doc_id
                            image_data = processor._process_images_for_doc(doc_id, chunks, image_dir)

                            if image_data:
                                print(f"[{doc_id}] Embedding and storing {len(image_data)} images...")
                                processor._embed_and_store_images(doc_id, image_data)
                                print(f"[{doc_id}] Successfully embedded and stored {len(image_data)} images")
                            else:
                                print(f"[{doc_id}] No image data to embed (images may not be referenced in chunks)")
                        except Exception as e:
                            print(f"[{doc_id}] Warning: Image embedding failed: {e}")
                            # Non-fatal error, continue with document processing
                else:
                    chunks_added_count = 0
                    print(f"[{doc_id}] Skipping embedding/storing: No embedder or vector store")
            
            # Fallback enrichment: if only LLM sources, retry enrichment with markdown
            existing_sources = final_metadata.get('metadata_sources', [])
            if not any(s in existing_sources for s in ['crossref', 'openlibrary', 'openalex', 'regex_detection']):
                try:
                    from services.metadata_enrichment import enrich_metadata, prepare_text_sample
                    from services import metadata_enrichment
                    import asyncio as _aio

                    md_sample = prepare_text_sample(markdown_content) if markdown_content else ""
                    if md_sample and len(md_sample) > 100:
                        async def _fallback_enrich():
                            metadata_enrichment._http_client = None
                            return await enrich_metadata(
                                existing_metadata=final_metadata,
                                document_text=md_sample,
                                filename=original_filename,
                            )

                        import concurrent.futures
                        with concurrent.futures.ThreadPoolExecutor() as pool:
                            enriched = pool.submit(lambda: _aio.run(_fallback_enrich())).result(timeout=30)
                        if enriched:
                            new_sources = enriched.get('metadata_sources', [])
                            if len(new_sources) > len(existing_sources):
                                final_metadata.update(enriched)
                                print(f"[{doc_id}] Fallback enrichment succeeded: sources={new_sources}")
                except Exception as e:
                    print(f"[{doc_id}] Fallback enrichment failed (non-fatal): {e}")

            # Finalize: normalize titles, classify type, score completeness
            from services.metadata_enrichment import finalize_metadata
            final_metadata = finalize_metadata(final_metadata, original_filename)

            # Generate description if missing -- use LLM to summarize from first chunks
            if not final_metadata.get('abstract') and not final_metadata.get('description'):
                try:
                    if processor.metadata_extractor and processor.metadata_extractor.client:
                        title = final_metadata.get('title', original_filename)
                        # Take first ~2000 chars of markdown, skip images/headers
                        sample_lines = []
                        for line in (markdown_content or "").split("\n"):
                            stripped = line.strip()
                            if not stripped or stripped.startswith("!["):
                                continue
                            if stripped.startswith("#") and len(sample_lines) == 0:
                                continue  # skip title heading
                            sample_lines.append(stripped)
                            if sum(len(l) for l in sample_lines) > 2000:
                                break
                        sample_text = "\n".join(sample_lines)

                        if sample_text and len(sample_text) > 100:
                            desc_response = processor.metadata_extractor.client.chat.completions.create(
                                model=processor.metadata_extractor.model,
                                messages=[{
                                    "role": "user",
                                    "content": f"Write a concise 2-3 sentence summary/description of this document in the same language as the text. Do not start with 'This document' or 'This paper'. Just describe what it covers.\n\nTitle: {title}\n\nText excerpt:\n{sample_text[:2000]}"
                                }],
                                max_tokens=200,
                                temperature=0.3,
                                timeout=15,
                            )
                            desc = desc_response.choices[0].message.content.strip()
                            if desc and len(desc) > 20:
                                final_metadata['abstract'] = desc
                                print(f"[{doc_id}] Generated description: {desc[:80]}...")
                except Exception as e_desc:
                    print(f"[{doc_id}] Description generation failed (non-fatal): {e_desc}")

            processing_result = {
                "doc_id": doc_id,
                "original_filename": original_filename,
                "chunks_generated": len(chunks),
                "chunks_added_to_vector_store": chunks_added_count,
                "extracted_metadata": final_metadata
            }
            
            print(f"[{doc_id}] Processing completed successfully!")
            print(f"[{doc_id}] Generated {processing_result.get('chunks_generated', 0)} chunks")
            print(f"[{doc_id}] Added {processing_result.get('chunks_added_to_vector_store', 0)} chunks to vector store")
            
            # Step 6: Complete (100% progress)
            self._update_job_progress_sync(job_id, user_id, 100, "completed")
            self._update_document_progress_sync(doc_id, user_id, 100, "completed")
            
            # Update chunk count in the database
            db_temp = next(get_db())
            try:
                crud.update_document_status(db_temp, doc_id, user_id, "completed", 100, 
                                           chunk_count=chunks_added_count)
            finally:
                db_temp.close()
            
            # Update document metadata with processing results
            db = next(get_db())
            try:
                document = crud.get_document(db, doc_id=doc_id, user_id=user_id)
                if document:
                    # Get extracted metadata
                    extracted_metadata = processing_result.get('extracted_metadata', {})
                    
                    # Preserve existing metadata (like file_hash) and merge with new metadata
                    existing_metadata = document.metadata_ or {}
                    
                    # Format metadata for UI expectations
                    formatted_metadata = {
                        "title": extracted_metadata.get('title'),
                        "authors": extracted_metadata.get('authors'),
                        "publication_year": extracted_metadata.get('publication_year') or extracted_metadata.get('year'),
                        "journal_or_source": extracted_metadata.get('journal_or_source') or extracted_metadata.get('journal'),
                        "abstract": extracted_metadata.get('abstract') or extracted_metadata.get('description'),
                        "doi": extracted_metadata.get('doi'),
                        "isbn": extracted_metadata.get('isbn'),
                        "publisher": extracted_metadata.get('publisher'),
                        "edition": extracted_metadata.get('edition'),
                        "organization": extracted_metadata.get('organization'),
                        "website_name": extracted_metadata.get('website_name'),
                        "document_type": extracted_metadata.get('document_type'),
                        "keywords": extracted_metadata.get('keywords'),
                        "metadata_completeness": extracted_metadata.get('metadata_completeness'),
                        "metadata_sources": extracted_metadata.get('metadata_sources'),
                        "processed_at": datetime.utcnow().isoformat(),
                        "processing_job_id": job_id,
                        "status": "completed",
                        "chunks_generated": processing_result.get('chunks_generated', 0),
                        "chunks_added_to_vector_store": processing_result.get('chunks_added_to_vector_store', 0)
                    }
                    
                    # Merge existing metadata with new metadata, preserving important fields like file_hash
                    merged_metadata = {**existing_metadata, **formatted_metadata}
                    
                    # Store in metadata_ field which UI expects
                    document.metadata_ = merged_metadata
                    
                    # Debug: Print what we're saving
                    print(f"[{doc_id}] Saving formatted metadata to database:")
                    print(f"  - Title: {merged_metadata.get('title')}")
                    print(f"  - Authors: {merged_metadata.get('authors')}")
                    print(f"  - Journal: {merged_metadata.get('journal_or_source')}")
                    print(f"  - Year: {merged_metadata.get('publication_year')}")
                    print(f"  - File Hash: {merged_metadata.get('file_hash', 'NOT SET')}")
                    
                    # Also set title and authors at top level if columns exist (for schema compatibility)
                    if hasattr(document, 'title') and formatted_metadata.get('title'):
                        document.title = formatted_metadata['title']
                    if hasattr(document, 'authors') and formatted_metadata.get('authors'):
                        authors = formatted_metadata['authors']
                        document.authors = json.dumps(authors) if isinstance(authors, list) else str(authors)
                    
                    # Update chunk_count with actual number of chunks added
                    if hasattr(document, 'chunk_count'):
                        document.chunk_count = processing_result.get('chunks_added_to_vector_store', 0)
                    
                    db.commit()
                    print(f"[{doc_id}] Updated document metadata in database")
            except Exception as e:
                print(f"Error updating document metadata: {e}")
            finally:
                db.close()
            
            return True
            
        except Exception as e:
            error_msg = f"Processing failed: {str(e)}"
            print(f"[{doc_id}] Document processing error: {error_msg}")
            print(traceback.format_exc())
            
            # Update status to failed
            self._update_job_progress_sync(job_id, user_id, 0, "failed", error_msg)
            self._update_document_progress_sync(doc_id, user_id, 0, "failed", error_msg)
            
            return False
            
        finally:
            # This part is handled by the main worker loop now
            pass

    def shutdown(self):
        """Shutdown the background processor."""
        print("Setting shutdown event for background processor.")
        self.shutdown_event.set()

# Global instance
background_processor = BackgroundDocumentProcessor()

if __name__ == "__main__":
    print("Background document processor service starting.")
    try:
        background_processor.start()
    except KeyboardInterrupt:
        print("Shutdown signal received. Stopping processor...")
    finally:
        background_processor.shutdown()
        print("Background processor shut down.")
