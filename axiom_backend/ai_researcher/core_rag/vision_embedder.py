import os
from typing import List, Dict, Any, Optional
import numpy as np
import torch
from sentence_transformers import SentenceTransformer
from PIL import Image
from tqdm import tqdm
import gc
import threading
import time
import asyncio
import logging
import sys
sys.path.append(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
try:
    from ai_researcher.hardware_detection import hardware_detector
except ImportError:
    from hardware_detection import hardware_detector

logger = logging.getLogger(__name__)

# Global semaphore to limit concurrent embedding operations
_vision_embedding_semaphore = None

def get_vision_embedding_semaphore():
    """Get or create the global vision embedding semaphore."""
    global _vision_embedding_semaphore
    if _vision_embedding_semaphore is None:
        from ai_researcher import config
        max_concurrent = config.EMBEDDING_MAX_CONCURRENT_QUERIES
        _vision_embedding_semaphore = asyncio.Semaphore(max_concurrent)
        logger.debug(f"Created vision embedding semaphore with limit: {max_concurrent}")
    return _vision_embedding_semaphore

class VisionEmbedder:
    """
    Generates dense vector embeddings for images using CLIP models.
    Includes GPU memory management and cleanup to prevent CUDA OOM errors.
    """
    def __init__(
        self,
        model_name: str = "clip-ViT-B-32",
        device: Optional[str] = None,
        batch_size: Optional[int] = None,
        enable_memory_management: Optional[bool] = None
    ):
        # Use device from config if available, otherwise use provided device or fallback
        import os
        from ai_researcher import config

        # Use config values if not explicitly provided
        if batch_size is None:
            batch_size = config.IMAGE_EMBEDDING_BATCH_SIZE
        if enable_memory_management is None:
            enable_memory_management = config.EMBEDDING_MEMORY_MANAGEMENT

        # Use hardware detector for device selection
        if device:
            self.device = device
        else:
            # Get device from per-model device config
            self.device = hardware_detector.get_model_device("vision_embedder")

            # Adjust batch size based on hardware (use smaller batch for images)
            optimal_batch = hardware_detector.get_optimal_batch_size(batch_size)
            if optimal_batch != batch_size:
                logger.info(f"Adjusting image batch size from {batch_size} to {optimal_batch} based on hardware")
                batch_size = optimal_batch

        self.model_name = model_name
        self.batch_size = batch_size
        self.enable_memory_management = enable_memory_management
        self.embedding_dim = 512  # CLIP ViT-B/32 produces 512-dimensional embeddings

        # Thread lock for model access to prevent concurrent GPU operations
        self._model_lock = threading.Lock()

        # Memory management settings
        self._memory_cleanup_threshold = 0.85  # Clean up when GPU memory usage exceeds 85%
        self._queries_since_cleanup = 0
        self._cleanup_frequency = 10  # Force cleanup every N queries

        # Log hardware detection results
        hardware_detector.log_device_info()

        logger.debug(f"Initializing VisionEmbedder with model {self.model_name} on device {self.device}")
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
            # Initialize the CLIP model
            logger.debug("Attempting to load CLIP model from Hugging Face...")
            self.model = SentenceTransformer(self.model_name, device=self.device)
            logger.debug("CLIP model loaded successfully.")

            # Initial memory cleanup
            if self.enable_memory_management:
                self._cleanup_gpu_memory()

        except Exception as e:
            logger.debug(f"Error loading vision embedding model {self.model_name}: {e}")
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

    def _load_image_safe(self, image_path: str) -> Optional[Image.Image]:
        """Safely load an image from path, handling errors gracefully."""
        try:
            img = Image.open(image_path).convert('RGB')
            return img
        except Exception as e:
            logger.warning(f"Failed to load image {image_path}: {e}")
            return None

    def embed_images(self, image_paths: List[str]) -> List[np.ndarray]:
        """
        Generates embeddings for a list of image paths.
        Includes memory management to prevent CUDA OOM errors.

        Args:
            image_paths: A list of file paths to images.

        Returns:
            A list of numpy arrays representing image embeddings.
            Returns zero vectors for corrupted or missing images.
        """
        if not image_paths:
            return []

        with self._model_lock:  # Ensure thread-safe access to the model
            num_images = len(image_paths)
            logger.debug(f"Generating embeddings for {num_images} images in batches of {self.batch_size}...")

            all_embeddings = []

            # Pre-embedding memory check
            if self.enable_memory_management:
                initial_usage = self._get_gpu_memory_usage()
                logger.debug(f"GPU memory before embedding: {initial_usage:.1%}")

            for i in tqdm(range(0, num_images, self.batch_size), desc="Embedding Images"):
                batch_paths = image_paths[i : i + self.batch_size]

                try:
                    # Memory check before each batch
                    if self.enable_memory_management and i > 0:
                        current_usage = self._get_gpu_memory_usage()
                        if current_usage > self._memory_cleanup_threshold:
                            logger.debug(f"High GPU memory usage ({current_usage:.1%}) detected. Cleaning up...")
                            self._cleanup_gpu_memory(force=True)

                    # Load images from batch
                    batch_images = []
                    valid_indices = []
                    for idx, path in enumerate(batch_paths):
                        img = self._load_image_safe(path)
                        if img is not None:
                            batch_images.append(img)
                            valid_indices.append(idx)

                    # Generate embeddings for valid images
                    if batch_images:
                        batch_embeddings = self.model.encode(
                            batch_images,
                            convert_to_numpy=True,
                            show_progress_bar=False
                        )

                        # Ensure batch_embeddings is numpy array
                        batch_embeddings = np.array(batch_embeddings, dtype=np.float32)
                    else:
                        batch_embeddings = np.array([])

                    # Create result array with zero vectors for failed images
                    batch_results = []
                    valid_idx = 0
                    for idx in range(len(batch_paths)):
                        if idx in valid_indices and valid_idx < len(batch_embeddings):
                            batch_results.append(batch_embeddings[valid_idx])
                            valid_idx += 1
                        else:
                            # Use zero vector as placeholder for corrupted images
                            zero_vector = np.zeros(self.embedding_dim, dtype=np.float32)
                            batch_results.append(zero_vector)
                            logger.warning(f"Using zero vector for failed image: {batch_paths[idx]}")

                    all_embeddings.extend(batch_results)

                    # Periodic cleanup during large batch processing
                    if self.enable_memory_management and (i // self.batch_size) % 5 == 0:
                        hardware_detector.empty_cache()

                except Exception as e:
                    logger.debug(f"Error embedding image batch starting at index {i}: {e}")
                    # Add zero vectors as placeholders for the entire batch
                    zero_vector = np.zeros(self.embedding_dim, dtype=np.float32)
                    all_embeddings.extend([zero_vector] * len(batch_paths))

            # Final memory cleanup
            if self.enable_memory_management:
                self._cleanup_gpu_memory()

            logger.debug("Finished generating image embeddings.")
            return all_embeddings

    def embed_single_image(self, image_path: str) -> Optional[np.ndarray]:
        """
        Generates embedding for a single image.
        Includes memory management to prevent CUDA OOM errors.

        Args:
            image_path: The file path to the image.

        Returns:
            A numpy array representing the image embedding,
            or None if embedding fails.
        """
        if not image_path:
            return None

        with self._model_lock:  # Ensure thread-safe access to the model
            # Increment query counter and check for cleanup
            self._queries_since_cleanup += 1

            # Periodic cleanup based on query count
            if (self.enable_memory_management and
                self._queries_since_cleanup >= self._cleanup_frequency):
                self._cleanup_gpu_memory(force=True)

            try:
                # Pre-query memory check
                if self.enable_memory_management:
                    current_usage = self._get_gpu_memory_usage()
                    if current_usage > self._memory_cleanup_threshold:
                        logger.debug(f"High GPU memory usage ({current_usage:.1%}) before image embedding. Cleaning up...")
                        self._cleanup_gpu_memory(force=True)

                # Load image
                img = self._load_image_safe(image_path)
                if img is None:
                    logger.warning(f"Failed to load image: {image_path}")
                    return np.zeros(self.embedding_dim, dtype=np.float32)

                # Generate embedding
                embedding = self.model.encode(
                    img,
                    convert_to_numpy=True,
                    show_progress_bar=False
                )

                embedding = np.array(embedding, dtype=np.float32)

                # Post-query cleanup for single images (lighter cleanup)
                if self.enable_memory_management:
                    hardware_detector.empty_cache()

                return embedding

            except RuntimeError as re:
                if hardware_detector.is_oom_error(re):
                    logger.debug(f"GPU OOM error during image embedding: {re}")
                    logger.debug(f"Attempting emergency GPU cleanup and retry for image: '{image_path}'")

                    # Emergency cleanup
                    if self.enable_memory_management:
                        hardware_detector.empty_cache()
                        gc.collect()
                        time.sleep(0.5)  # Give more time for cleanup

                        # Try once more
                        try:
                            img = self._load_image_safe(image_path)
                            if img:
                                embedding = self.model.encode(
                                    img,
                                    convert_to_numpy=True,
                                    show_progress_bar=False
                                )
                                embedding = np.array(embedding, dtype=np.float32)
                                logger.debug(f"Successfully recovered from GPU OOM for image: '{image_path}'")
                                return embedding
                        except Exception as retry_error:
                            logger.debug(f"Retry after GPU OOM also failed: {retry_error}")

                    return np.zeros(self.embedding_dim, dtype=np.float32)
                else:
                    # Re-raise non-OOM RuntimeErrors
                    raise
            except Exception as e:
                logger.debug(f"Error embedding image '{image_path}': {e}")
                import traceback
                traceback.print_exc()
                return np.zeros(self.embedding_dim, dtype=np.float32)

    async def embed_images_async(self, image_paths: List[str]) -> List[np.ndarray]:
        """
        Async wrapper for embed_images that uses a semaphore to limit concurrent operations.
        This helps prevent GPU memory overload when multiple batches are processed simultaneously.
        """
        if not image_paths:
            return []

        semaphore = get_vision_embedding_semaphore()
        async with semaphore:
            # Run the synchronous embedding in a thread pool to avoid blocking
            return await asyncio.get_running_loop().run_in_executor(None, self.embed_images, image_paths)

    async def embed_single_image_async(self, image_path: str) -> Optional[np.ndarray]:
        """
        Async wrapper for embed_single_image that uses a semaphore to limit concurrent operations.
        This helps prevent GPU memory overload when multiple queries are processed simultaneously.
        """
        if not image_path:
            return None

        semaphore = get_vision_embedding_semaphore()
        async with semaphore:
            # Run the synchronous embedding in a thread pool to avoid blocking
            return await asyncio.get_running_loop().run_in_executor(None, self.embed_single_image, image_path)

    def __del__(self):
        """Cleanup when the embedder is destroyed."""
        if hasattr(self, 'enable_memory_management') and self.enable_memory_management:
            try:
                self._cleanup_gpu_memory(force=True)
            except:
                pass  # Ignore errors during cleanup
