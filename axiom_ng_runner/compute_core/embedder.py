import asyncio
import contextlib
import gc
import logging
import os
import sys
import threading
import time
from typing import Any

import numpy as np
import torch
from FlagEmbedding import BGEM3FlagModel
from tqdm import tqdm

sys.path.append(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
try:
    from axiom_ng_runner.compute_core.devices import hardware_detector
except ImportError:
    from hardware_detection import hardware_detector

logger = logging.getLogger(__name__)

# Global semaphore to limit concurrent embedding operations
_embedding_semaphore = None

def get_embedding_semaphore():
    """Get or create the global embedding semaphore."""
    global _embedding_semaphore
    if _embedding_semaphore is None:
        # Env with the old config.py production defaults (semaphore cap).
        max_concurrent = int(os.getenv("EMBEDDING_MAX_CONCURRENT_QUERIES", "3"))
        _embedding_semaphore = asyncio.Semaphore(max_concurrent)
        logger.debug(f"Created embedding semaphore with limit: {max_concurrent}")
    return _embedding_semaphore

class TextEmbedder:
    """
    Generates dense and sparse vector embeddings for text chunks using BGE-M3.
    Includes GPU memory management and cleanup to prevent CUDA OOM errors.
    """
    def __init__(
        self,
        model_name: str = "BAAI/bge-m3",
        device: str | None = None,
        batch_size: int | None = None,
        max_length: int = 8192, # Max sequence length for BGE-M3
        enable_memory_management: bool | None = None
    ):
        # Use device from config if available, otherwise use provided device or fallback
        import os

        # Env with the old config.py production defaults.
        if batch_size is None:
            batch_size = int(os.getenv("EMBEDDING_BATCH_SIZE", "8"))
        if enable_memory_management is None:
            enable_memory_management = os.getenv("EMBEDDING_MEMORY_MANAGEMENT", "True").lower() == "true"

        # Use hardware detector for device selection
        if device:
            self.device = device
        else:
            # Get device from per-model device config
            self.device = hardware_detector.get_model_device("embedder")

            # Adjust batch size based on hardware
            optimal_batch = hardware_detector.get_optimal_batch_size(batch_size)
            if optimal_batch != batch_size:
                logger.info(f"Adjusting batch size from {batch_size} to {optimal_batch} based on hardware")
                batch_size = optimal_batch

        self.model_name = model_name
        self.batch_size = batch_size
        self.max_length = max_length
        self.enable_memory_management = enable_memory_management

        # Thread lock for model access to prevent concurrent GPU operations
        self._model_lock = threading.Lock()

        # Memory management settings
        self._memory_cleanup_threshold = 0.85  # Clean up when GPU memory usage exceeds 85%
        self._queries_since_cleanup = 0
        self._cleanup_frequency = 10  # Force cleanup every N queries

        # Log hardware detection results
        hardware_detector.log_device_info()

        logger.debug(f"Initializing TextEmbedder with model {self.model_name} on device {self.device}")
        logger.debug(f"Memory management: {'Enabled' if self.enable_memory_management else 'Disabled'}")

        # Set PyTorch memory allocation strategy for better memory management
        device_info = hardware_detector.detect_hardware()
        if device_info["device_type"] in ["cuda", "rocm"] and self.enable_memory_management:
            # Enable expandable segments to reduce fragmentation
            os.environ['PYTORCH_CUDA_ALLOC_CONF'] = 'expandable_segments:True'
            logger.debug("Set PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True for better memory management")
        elif device_info["device_type"] == "cpu":
            # CPU-specific optimizations
            torch.set_num_threads(hardware_detector.get_num_workers())
            logger.debug(f"Set PyTorch threads to {hardware_detector.get_num_workers()} for CPU processing")

        try:
            # Initialize the BGE-M3 model
            logger.debug("Attempting to load BGE-M3 model from Hugging Face...")
            self.model = BGEM3FlagModel(
                self.model_name,
                # Force fp32 to avoid dtype issues downstream (e.g., in vector store)
                use_fp16=False
            )
            logger.debug("BGE-M3 model loaded successfully (forced fp32).")

            # Initial memory cleanup
            if self.enable_memory_management:
                self._cleanup_gpu_memory()

        except Exception as e:
            logger.debug(f"Error loading embedding model {self.model_name}: {e}")
            # Potentially raise the error or handle it depending on desired robustness
            raise

    def _get_gpu_memory_usage(self) -> float:
        """Get current GPU memory usage as a percentage."""
        try:
            total = hardware_detector.get_total_memory()
            if total == 0:
                return 0.0
            allocated = hardware_detector.memory_allocated()
            reserved = hardware_detector.memory_reserved()
            return (allocated + reserved) / total
        except Exception as e:
            logger.debug(f"Warning: Could not get GPU memory usage: {e}")
            return 0.0

    def _cleanup_gpu_memory(self, force: bool = False):
        """Clean up GPU memory to prevent OOM errors."""
        device_info = hardware_detector.detect_hardware()
        if not self.enable_memory_management or device_info["device_type"] == "cpu":
            return

        try:
            current_usage = self._get_gpu_memory_usage()

            if force or current_usage > self._memory_cleanup_threshold:
                logger.debug(f"GPU memory usage: {current_usage:.1%}. Performing cleanup...")

                hardware_detector.empty_cache()
                gc.collect()
                time.sleep(0.1)

                new_usage = self._get_gpu_memory_usage()
                logger.debug(f"GPU memory after cleanup: {new_usage:.1%}")

                self._queries_since_cleanup = 0

        except Exception as e:
            logger.debug(f"Warning: GPU memory cleanup failed: {e}")

    def embed_chunks(self, chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """
        Generates dense and sparse embeddings for a list of text chunks.
        Includes memory management to prevent CUDA OOM errors.
        Prepends section titles to chunk text for better embedding context.

        Args:
            chunks: A list of chunk dictionaries, each expected to have a 'text' key.

        Returns:
            The same list of chunks, with an 'embeddings' dictionary added to each,
            containing 'dense' and 'sparse' vectors. Returns an empty list if input is empty.
        """
        if not chunks:
            return []

        with self._model_lock:  # Ensure thread-safe access to the model
            # Prepend section titles to text for better embedding context
            all_texts = []
            for chunk in chunks:
                text = chunk["text"]
                titles = chunk.get("metadata", {}).get("section_titles", [])
                if titles:
                    prefix = "> Section: " + " > ".join(titles)
                    text = f"{prefix}\n\n{text}"
                all_texts.append(text)

            num_chunks = len(all_texts)
            logger.debug(f"Generating embeddings for {num_chunks} chunks in batches of {self.batch_size}...")

            # Store results temporarily
            dense_embeddings = []
            sparse_embeddings = [] # Using list of dicts for sparse representation

            # Pre-embedding memory check
            if self.enable_memory_management:
                initial_usage = self._get_gpu_memory_usage()
                logger.debug(f"GPU memory before embedding: {initial_usage:.1%}")

            for i in tqdm(range(0, num_chunks, self.batch_size), desc="Embedding Chunks"):
                batch_texts = all_texts[i : i + self.batch_size]

                try:
                    # Memory check before each batch
                    if self.enable_memory_management and i > 0:
                        current_usage = self._get_gpu_memory_usage()
                        if current_usage > self._memory_cleanup_threshold:
                            logger.debug(f"High GPU memory usage ({current_usage:.1%}) detected. Cleaning up...")
                            self._cleanup_gpu_memory(force=True)

                    # Generate embeddings using the BGE-M3 model
                    # We need both dense and sparse (lexical_weights)
                    outputs = self.model.encode(
                        batch_texts,
                        batch_size=len(batch_texts), # Pass current batch size
                        max_length=self.max_length,
                        return_dense=True,
                        return_sparse=True,
                        return_colbert_vecs=False # Not requested for now
                    )

                    # Ensure outputs are numpy arrays for consistency before converting
                    batch_dense = np.array(outputs["dense_vecs"], dtype=np.float32)
                    batch_sparse_dicts = outputs["lexical_weights"] # List of dictionaries

                    dense_embeddings.extend(batch_dense.tolist()) # Store as lists
                    sparse_embeddings.extend(batch_sparse_dicts)

                    # Periodic cleanup during large batch processing
                    if self.enable_memory_management and (i // self.batch_size) % 5 == 0:
                        hardware_detector.empty_cache()

                except Exception as e:
                    logger.debug(f"Error embedding batch starting at index {i}: {e}")
                    # Handle error: skip batch, add placeholders, or re-raise
                    # Adding placeholders for now to maintain list length alignment
                    # Attempt to get hidden size safely
                    hidden_size = getattr(getattr(self.model, 'model', None), 'config', None).hidden_size if hasattr(self.model, 'model') else 1024 # Default fallback size
                    error_placeholder_dense = [0.0] * hidden_size
                    error_placeholder_sparse = {}
                    dense_embeddings.extend([error_placeholder_dense] * len(batch_texts))
                    sparse_embeddings.extend([error_placeholder_sparse] * len(batch_texts))

            # Final memory cleanup
            if self.enable_memory_management:
                self._cleanup_gpu_memory()

            # Check if the number of embeddings matches the number of chunks
            if len(dense_embeddings) != num_chunks or len(sparse_embeddings) != num_chunks:
                 logger.debug(f"Error: Mismatch between number of chunks ({num_chunks}) and generated embeddings ({len(dense_embeddings)} dense, {len(sparse_embeddings)} sparse).")
                 # Handle this critical error, maybe return None or raise exception
                 # For now, we'll proceed but this indicates a problem
                 # Attempt to pad if lengths are mismatched (less ideal)
                 hidden_size = getattr(getattr(self.model, 'model', None), 'config', None).hidden_size if hasattr(self.model, 'model') else 1024 # Default fallback size
                 error_placeholder_dense = [0.0] * hidden_size
                 error_placeholder_sparse = {}
                 while len(dense_embeddings) < num_chunks: dense_embeddings.append(error_placeholder_dense)
                 while len(sparse_embeddings) < num_chunks: sparse_embeddings.append(error_placeholder_sparse)
                 dense_embeddings = dense_embeddings[:num_chunks]
                 sparse_embeddings = sparse_embeddings[:num_chunks]

            # Add embeddings back to the original chunk dictionaries
            for i, chunk in enumerate(chunks):
                chunk["embeddings"] = {
                    "dense": dense_embeddings[i],
                    "sparse": sparse_embeddings[i] # Store sparse as dict {token_id: weight}
                }

            logger.debug("Finished generating embeddings.")
            return chunks

    def embed_query(self, query_text: str) -> dict[str, Any] | None:
        """
        Generates dense and sparse embeddings for a single query text.
        Includes memory management to prevent CUDA OOM errors.

        Args:
            query_text: The query string.

        Returns:
            A dictionary containing 'dense' and 'sparse' query embeddings,
            or None if embedding fails.
        """
        if not query_text:
            return None

        with self._model_lock:  # Ensure thread-safe access to the model
            # Increment query counter and check for cleanup
            self._queries_since_cleanup += 1

            # Periodic cleanup based on query count
            if (self.enable_memory_management and
                self._queries_since_cleanup >= self._cleanup_frequency):
                self._cleanup_gpu_memory(force=True)

            outputs = None # Initialize outputs
            try:
                # Pre-query memory check
                if self.enable_memory_management:
                    current_usage = self._get_gpu_memory_usage()
                    if current_usage > self._memory_cleanup_threshold:
                        logger.debug(f"High GPU memory usage ({current_usage:.1%}) before query embedding. Cleaning up...")
                        self._cleanup_gpu_memory(force=True)

                # --- Wrap the encode call itself ---
                logger.info(f"embed_query: Calling model.encode for '{query_text[:50]}', model type={type(self.model).__name__}, max_length={self.max_length}")
                outputs = self.model.encode(
                    [query_text], # Encode as a list with one item
                    max_length=self.max_length,
                    return_dense=True,
                    return_sparse=True,
                    return_colbert_vecs=False
                )
                logger.info(f"embed_query: model.encode returned, outputs keys={list(outputs.keys()) if outputs else None}")
                # --- End wrap ---

                # --- Enhanced checks and logging ---
                # This print might still not be reached if error is inside encode
                # logger.debug(f"DEBUG: Raw outputs from model.encode for query '{query_text}': {outputs}")

                if outputs is None: # Check if encode failed and returned None implicitly (though unlikely)
                     logger.error(f"embed_query: model.encode returned None for query '{query_text}'")
                     return None

                dense_vecs = outputs.get("dense_vecs")
                lexical_weights = outputs.get("lexical_weights")

                # Explicitly check if the lists are None or empty
                if dense_vecs is None or len(dense_vecs) == 0:
                    logger.error(f"embed_query: Embedding model returned empty or None dense vectors for query: '{query_text}'. Dense vecs type: {type(dense_vecs)}")
                    return None

                if lexical_weights is None or len(lexical_weights) == 0:
                    logger.error(f"embed_query: Embedding model returned empty or None lexical weights for query: '{query_text}'. Lexical weights type: {type(lexical_weights)}")
                    return None
                # --- End enhanced checks ---

                # Proceed only if checks pass
                dense_vec = np.array(dense_vecs[0], dtype=np.float32).tolist()
                sparse_dict = lexical_weights[0] # Dictionary {token_id: weight}

                # Post-query cleanup for single queries (lighter cleanup)
                if self.enable_memory_management:
                    hardware_detector.empty_cache()

                return {
                    "dense": dense_vec,
                    "sparse": sparse_dict
                }
            # --- Catch specific IndexError from encode() ---
            except IndexError as ie:
                logger.error(f"embed_query: IndexError from model.encode() for query '{query_text}'. Outputs: {outputs}. Error: {ie}", exc_info=True)
                # Also log the full traceback if possible, though print might be enough here
                import traceback
                traceback.print_exc()
                return None
            # --- Catch GPU OOM specifically ---
            except RuntimeError as re:
                if hardware_detector.is_oom_error(re):
                    logger.error(f"embed_query: GPU OOM error during query embedding: {re}")
                    logger.error(f"embed_query: Attempting emergency GPU cleanup and retry for query: '{query_text}'")

                    # Emergency cleanup
                    if self.enable_memory_management:
                        hardware_detector.empty_cache()
                        gc.collect()
                        time.sleep(0.5)  # Give more time for cleanup

                        # Try once more with reduced batch size
                        try:
                            outputs = self.model.encode(
                                [query_text],
                                max_length=min(self.max_length, 4096),  # Reduce max length
                                return_dense=True,
                                return_sparse=True,
                                return_colbert_vecs=False
                            )

                            if outputs and outputs.get("dense_vecs") and outputs.get("lexical_weights"):
                                dense_vec = np.array(outputs["dense_vecs"][0], dtype=np.float32).tolist()
                                sparse_dict = outputs["lexical_weights"][0]
                                logger.debug(f"Successfully recovered from GPU OOM for query: '{query_text}'")
                                return {"dense": dense_vec, "sparse": sparse_dict}
                        except Exception as retry_error:
                            logger.debug(f"Retry after GPU OOM also failed: {retry_error}")

                    return None
                else:
                    # Re-raise non-OOM RuntimeErrors
                    raise
            # --- Catch other potential exceptions ---
            except Exception as e:
                logger.error(f"embed_query: Error embedding query '{query_text}': {e}", exc_info=True)
                return None

    def embed_queries_dense(self, texts: list[str]) -> list[list[float]]:
        """Dense-only query embeddings for POST /v1/embed (epic #130 R1).

        BGE-M3 is a symmetric encoder: passages and queries go through the
        same model and pooling, so query vectors are cosine-comparable with
        the chunk embeddings this same class produces at ingest (the OS
        roundtrip integration test is the proof). Deliberately skips the
        per-call GPU cache flush and cleanup counters of embed_query() —
        the warm path must hold the <150 ms p95 budget.
        """
        with self._model_lock:
            outputs = self.model.encode(
                texts,
                batch_size=len(texts),
                max_length=512,  # queries are short; FlagEmbedding's own
                # query_max_length default is 512 too. 8192 would only pad.
                return_dense=True,
                return_sparse=False,
                return_colbert_vecs=False,
            )
        return np.asarray(outputs["dense_vecs"], dtype=np.float32).tolist()

    async def embed_query_async(self, query_text: str) -> dict[str, Any] | None:
        """
        Async wrapper for embed_query that uses a semaphore to limit concurrent operations.
        This helps prevent GPU memory overload when multiple queries are processed simultaneously.
        """
        if not query_text:
            return None

        semaphore = get_embedding_semaphore()
        async with semaphore:
            # Run the synchronous embedding in a thread pool to avoid blocking
            return await asyncio.get_running_loop().run_in_executor(None, self.embed_query, query_text)

    async def embed_chunks_async(self, chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """
        Async wrapper for embed_chunks that uses a semaphore to limit concurrent operations.
        This helps prevent GPU memory overload when multiple chunk batches are processed simultaneously.
        """
        if not chunks:
            return []

        semaphore = get_embedding_semaphore()
        async with semaphore:
            # Run the synchronous embedding in a thread pool to avoid blocking
            return await asyncio.get_running_loop().run_in_executor(None, self.embed_chunks, chunks)

    def __del__(self):
        """Cleanup when the embedder is destroyed."""
        if hasattr(self, 'enable_memory_management') and self.enable_memory_management:
            with contextlib.suppress(BaseException):
                self._cleanup_gpu_memory(force=True)  # best-effort: never raise in __del__
