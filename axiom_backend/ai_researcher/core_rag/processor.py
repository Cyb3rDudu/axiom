import os
import json
import time
import uuid
from pathlib import Path
from typing import List, Dict, Tuple, Optional, Any
import shutil # Import shutil at the top
import logging
import sys
import signal
from contextlib import contextmanager

import re
import pymupdf # PyMuPDF
import pymupdf4llm # For fallback or specific markdown conversion if needed later
# NOTE: torch, marker, FlagEmbedding, and hardware_detection are NOT
# imported at module level. They are lazy-loaded inside the methods
# that need them (see issue #14) so that the doc-processor's long-lived
# Python process never touches torch (saving ~1 GB idle RAM + CUDA context).


def extract_page_labels(pdf_path: str) -> Dict[int, str]:
    """
    Extract logical page labels from PDF with 3-tier fallback.

    Tier 1: PDF embedded page labels (publisher metadata)
    Tier 2: Header/footer page number parsing
    Tier 3: Physical page index + 1

    Returns: Dict mapping physical page index (0-based) to display label string.
    """
    doc = pymupdf.open(str(pdf_path))
    n = doc.page_count
    logger = logging.getLogger(__name__)

    # Tier 1: PDF page labels
    labels = {}
    empty_count = 0
    for i in range(n):
        label = doc[i].get_label()
        if label and label.strip():
            labels[i] = label.strip()
        else:
            empty_count += 1

    if empty_count < n * 0.5:
        logger.info(f"Page labels: Tier 1 (PDF labels) - {len(labels)}/{n} pages labeled")
        doc.close()
        return labels

    # Tier 2: Parse header/footer numbers
    labels = {}
    sample_pages = list(range(min(n, 20))) + list(range(max(0, n - 5), n))
    sample_pages = sorted(set(p for p in sample_pages if p < n))

    for i in sample_pages:
        page = doc[i]
        rect = page.rect
        # Bottom 8% of page
        footer_rect = pymupdf.Rect(rect.x0, rect.y1 * 0.92, rect.x1, rect.y1)
        footer_text = page.get_text("text", clip=footer_rect).strip()
        # Top 8% of page
        header_rect = pymupdf.Rect(rect.x0, rect.y0, rect.x1, rect.y1 * 0.08)
        header_text = page.get_text("text", clip=header_rect).strip()

        for text in [footer_text, header_text]:
            # Look for standalone number (1-4 digits)
            match = re.search(r'(?:^|\s)(\d{1,4})(?:\s|$)', text)
            if match:
                labels[i] = match.group(1)
                break

    # Validate: check if parsed numbers are roughly sequential
    parsed_nums = [(i, int(labels[i])) for i in sorted(labels) if labels.get(i, "").isdigit()]
    if len(parsed_nums) >= len(sample_pages) * 0.3:
        # Outlier detection: remove entries where the gap to neighbors is too large
        # e.g., [60, 536, 537, 538] → 60 is an outlier (volume number, not page)
        if len(parsed_nums) >= 3:
            cleaned = []
            for j, (phys, logical) in enumerate(parsed_nums):
                # Compare with next entry
                if j < len(parsed_nums) - 1:
                    next_logical = parsed_nums[j + 1][1]
                    gap = abs(next_logical - logical)
                else:
                    prev_logical = parsed_nums[j - 1][1]
                    gap = abs(logical - prev_logical)

                if gap > 20:
                    # Check if this is the outlier or the rest is
                    # Count how many entries are close to this one vs far
                    close_count = sum(1 for _, l in parsed_nums if abs(l - logical) <= 20)
                    if close_count < len(parsed_nums) * 0.5:
                        # This entry is the outlier, skip it
                        logger.debug(f"Page label outlier: physical={phys}, parsed={logical} (gap={gap})")
                        del labels[phys]
                        continue
                cleaned.append((phys, logical))
            parsed_nums = cleaned

        increasing = sum(1 for j in range(1, len(parsed_nums))
                        if parsed_nums[j][1] > parsed_nums[j - 1][1])
        if len(parsed_nums) >= 2 and increasing > len(parsed_nums) * 0.7:
            # Extrapolate to all pages based on sampled pattern
            offsets = [logical - physical for physical, logical in parsed_nums]
            median_offset = sorted(offsets)[len(offsets) // 2]
            full_labels = {}
            for i in range(n):
                val = i + median_offset
                if val < 1:
                    full_labels[i] = ""  # Front matter before page 1
                else:
                    full_labels[i] = str(val)
            # Override with actual parsed values (including Roman numerals)
            full_labels.update(labels)

            # Also try to parse Roman numerals from front matter headers
            _ROMAN_RE = re.compile(r'^((?:X{0,3})(?:IX|IV|V?I{0,3}))$', re.IGNORECASE)
            for i in range(min(n, 30)):
                if full_labels.get(i):
                    continue  # Already has a label
                page = doc[i]
                rect = page.rect
                header_rect = pymupdf.Rect(rect.x0, rect.y0, rect.x1, rect.y1 * 0.08)
                header_text = page.get_text("text", clip=header_rect).strip()
                footer_rect = pymupdf.Rect(rect.x0, rect.y1 * 0.92, rect.x1, rect.y1)
                footer_text = page.get_text("text", clip=footer_rect).strip()
                for text_block in [header_text, footer_text]:
                    for word in text_block.split():
                        if _ROMAN_RE.match(word.strip()):
                            full_labels[i] = word.strip().upper()
                            break
                    if full_labels.get(i):
                        break

            logger.info(f"Page labels: Tier 2 (header/footer) - offset={median_offset}")
            doc.close()
            return full_labels

    # Tier 3: Physical page + 1
    logger.info(f"Page labels: Tier 3 (physical index + 1)")
    labels = {i: str(i + 1) for i in range(n)}
    doc.close()
    return labels
import sqlite3 # Needed for Database integration (error handling)

# Torch-free components — safe to import at module level.
from .metadata_extractor import MetadataExtractor
from .chunker import Chunker
from .vector_store_singleton import get_vector_store
from .pgvector_store import PGVectorStore as VectorStore
from .document_converter import DocumentConverter

# Heavy imports (torch, marker, hardware_detection, embedder) are
# lazy-loaded inside __init__ / _init_marker_configs so importing this
# module doesn't pull torch into the process (issue #14).

# Set up logging for table processing
logger = logging.getLogger(__name__)

@contextmanager
def timeout_context(seconds):
    """
    Context manager for timeouts using signal.alarm().

    Usage:
        with timeout_context(32400):  # 9 hours
            result = some_long_running_operation()

    Raises:
        TimeoutError: If the operation exceeds the specified timeout
    """
    def timeout_handler(signum, frame):
        raise TimeoutError(f"Operation timed out after {seconds} seconds ({seconds/3600:.1f} hours)")

    # Save the old handler
    old_handler = signal.signal(signal.SIGALRM, timeout_handler)
    signal.alarm(seconds)

    try:
        yield
    finally:
        # Cancel the alarm and restore the old handler
        signal.alarm(0)
        signal.signal(signal.SIGALRM, old_handler)

class DocumentProcessor:
    """
    Handles processing of documents (PDF, Word, Markdown):
    1. Extracts initial text for metadata extraction.
    2. Converts documents to Markdown format (PDF->Markdown via marker, Word->Markdown via python-docx, Markdown direct).
    3. Assigns a unique document ID.
    4. Extracts structured metadata using an LLM.
    5. Chunks the Markdown content.
    6. Embeds and stores in vector store.
    """
    def __init__(
        self,
        pdf_dir: str | Path = "data/raw_pdfs",
        markdown_dir: str | Path = "data/processed/markdown",
        metadata_dir: str | Path = "data/processed/metadata",
        db_path: Optional[str | Path] = None,
        embedder=None,
        vector_store: Optional[VectorStore] = None,
        force_reembed: bool = False,
        device: Optional[str] = None,
        load_marker: bool = True,
    ):
        self.pdf_dir = Path(pdf_dir)
        self.markdown_dir = Path(markdown_dir)
        self.metadata_dir = Path(metadata_dir)
        db_path = Path(db_path) if db_path else Path("data/processed/metadata.db")

        self.markdown_dir.mkdir(parents=True, exist_ok=True)
        self.metadata_dir.mkdir(parents=True, exist_ok=True)
        db_path.parent.mkdir(parents=True, exist_ok=True)

        # Device selection and hardware detection are only needed when
        # loading Marker (which uses torch). When load_marker=False (the
        # doc-processor path), skip entirely — no torch import.
        if load_marker:
            try:
                from ai_researcher.hardware_detection import hardware_detector
            except ImportError:
                from hardware_detection import hardware_detector
            import torch

            if device:
                self.device = device
            else:
                self.device = hardware_detector.get_model_device("marker")

            hardware_detector.log_device_info()
            print(f"DocumentProcessor using device: {self.device}")

            device_info = hardware_detector.detect_hardware()
            if device_info["device_type"] == "cpu":
                torch.set_num_threads(hardware_detector.get_num_workers())
                print(f"Set PyTorch threads to {hardware_detector.get_num_workers()} for CPU processing")

            from marker.models import create_model_dict
            self.marker_models = create_model_dict(device=self.device)
            self._init_marker_configs()
        else:
            self.device = device or "cpu"
            self.marker_models = None
            self.table_converter = None
            self.no_table_converter = None
            self.marker_converter = None
            logger.info("DocumentProcessor: Marker + torch loading skipped (load_marker=False)")

        # Initialize other components
        self.metadata_extractor = MetadataExtractor()
        self.chunker = Chunker()
        self.document_converter = DocumentConverter()  # Initialize document converter
        self.embedder = embedder
        self.vector_store = vector_store
        self.force_reembed = force_reembed # Store the flag

        # Lazy loading for vision embedder
        self._vision_embedder = None
        from ai_researcher import config
        self.image_dir = Path("data/processed/images")
        self.image_dir.mkdir(parents=True, exist_ok=True)

        if self.embedder is None or self.vector_store is None:
            print("Warning: DocumentProcessor initialized without embedder or vector_store. Embedding/storing will be skipped.")
        if self.force_reembed:
            print("Warning: --force-reembed flag is active. All PDFs will be re-processed and re-embedded.")

    @property
    def vision_embedder(self):
        """Lazy load vision embedder only when needed."""
        if self._vision_embedder is None:
            from ai_researcher import config
            if config.ENABLE_IMAGE_EMBEDDINGS:
                from .vision_embedder import VisionEmbedder
                self._vision_embedder = VisionEmbedder(
                    model_name=config.IMAGE_EMBEDDING_MODEL,
                    device=self.device,
                    batch_size=config.IMAGE_EMBEDDING_BATCH_SIZE
                )
        return self._vision_embedder

    # --- Methods correctly indented within the class ---

    def _init_marker_configs(self):
        """Initialize different marker configurations for table handling."""
        try:
            from ai_researcher.hardware_detection import hardware_detector
        except ImportError:
            from hardware_detection import hardware_detector
        from marker.converters.pdf import PdfConverter
        from marker.config.parser import ConfigParser

        device_info = hardware_detector.detect_hardware()
        # Adjust batch multiplier based on hardware
        # Reduce batch multiplier to avoid overwhelming Ollama with concurrent vision requests
        batch_multiplier = 1

        from ai_researcher import config
        base_options = {
            "output_format": "markdown",
            "device": self.device,
            "batch_multiplier": batch_multiplier,
            "disable_image_extraction": not config.ENABLE_IMAGE_EXTRACTION,
            "disable_image_captions": True,  # Skip LLM captions (using CLIP embeddings instead)
            "paginate_output": True,  # Inject page markers for citation page numbers
        }

        # Add LLM support for enhanced OCR (formulas, tables, etc.)
        if config.MARKER_USE_LLM:
            base_options.update({
                "use_llm": True,
                "llm_service": config.MARKER_LLM_SERVICE,
            })

            # Configure based on service type
            if "ollama" in config.MARKER_LLM_SERVICE.lower():
                base_options.update({
                    "ollama_base_url": config.MARKER_OLLAMA_BASE_URL,
                    "ollama_model": config.MARKER_LLM_MODEL,
                })
                logger.info(f"Marker LLM enabled: {config.MARKER_LLM_MODEL} via Ollama at {config.MARKER_OLLAMA_BASE_URL}")
            else:
                # OpenAI-compatible service (fallback for other services)
                import os
                os.environ["OPENAI_API_KEY"] = getattr(config, "MARKER_LLM_API_KEY", "")
                os.environ["OPENAI_BASE_URL"] = getattr(config, "MARKER_LLM_BASE_URL", "")
                base_options.update({
                    "openai_api_key": getattr(config, "MARKER_LLM_API_KEY", ""),
                    "openai_base_url": getattr(config, "MARKER_LLM_BASE_URL", ""),
                    "openai_model": config.MARKER_LLM_MODEL,
                })
                logger.info(f"Marker LLM enabled: {config.MARKER_LLM_MODEL} via {getattr(config, 'MARKER_LLM_BASE_URL', 'OpenAI')}")
        else:
            logger.info("Marker LLM disabled (set MARKER_USE_LLM=True to enable)")
        
        # Configuration with table recognition enabled
        # Note: marker may use different option names, so we try multiple approaches
        table_options = base_options.copy()
        table_options.update({
            "extract_tables": True,
            "table_rec": True,
            "table_recognition": True,  # Alternative option name
            "enable_tables": True,      # Alternative option name
        })
        
        # Configuration with table recognition disabled (safe fallback)
        no_table_options = base_options.copy()
        no_table_options.update({
            "extract_tables": False,
            "table_rec": False,
            "table_recognition": False,
            "enable_tables": False,
        })
        
        # Create configurations
        self.table_config = ConfigParser(cli_options=table_options)
        self.no_table_config = ConfigParser(cli_options=no_table_options)
        
        # Log the actual configurations being used
        table_config_dict = self.table_config.generate_config_dict()
        no_table_config_dict = self.no_table_config.generate_config_dict()
        
        logger.info(f"Table config keys: {list(table_config_dict.keys())}")
        logger.info(f"No-table config keys: {list(no_table_config_dict.keys())}")
        
        # Create converters
        self.table_converter = PdfConverter(
            config=table_config_dict,
            artifact_dict=self.marker_models,
            processor_list=self.table_config.get_processors(),
            renderer=self.table_config.get_renderer(),
            llm_service=self.table_config.get_llm_service()
        )

        self.no_table_converter = PdfConverter(
            config=no_table_config_dict,
            artifact_dict=self.marker_models,
            processor_list=self.no_table_config.get_processors(),
            renderer=self.no_table_config.get_renderer(),
            llm_service=self.no_table_config.get_llm_service()
        )
        
        # Default to the safer no-table converter
        self.marker_converter = self.no_table_converter
        
        logger.info("Marker configurations initialized with table handling support")

    def _detect_tables(self, pdf_path: Path) -> bool:
        """
        Detect if the PDF contains tables using a lightweight approach.
        Returns True if tables are likely present, False otherwise.
        """
        try:
            logger.info(f"Detecting tables in {pdf_path.name}...")
            
            # Use PyMuPDF to do a quick scan for table-like structures
            doc = pymupdf.open(pdf_path)
            if not doc or len(doc) == 0:
                logger.warning(f"Could not open PDF for table detection: {pdf_path}")
                return False
            
            table_indicators = 0
            pages_to_check = min(3, len(doc))  # Check first 3 pages only
            
            for page_num in range(pages_to_check):
                page = doc[page_num]
                text = page.get_text()
                
                # Look for table indicators in the text
                # Count tab characters (common in table exports)
                tab_count = text.count('\t')
                if tab_count > 5:  # Threshold for tab-separated content
                    table_indicators += 1
                
                # Look for repeated patterns that suggest tabular data
                lines = text.split('\n')
                aligned_lines = 0
                for line in lines:
                    # Count lines with multiple spaces (column alignment)
                    if '  ' in line and len(line.split()) > 2:
                        aligned_lines += 1
                
                if aligned_lines > 5:  # Threshold for aligned content
                    table_indicators += 1
                
                # Look for table-related keywords
                table_keywords = ['table', 'column', 'row', 'data', 'figure']
                text_lower = text.lower()
                keyword_count = sum(1 for keyword in table_keywords if keyword in text_lower)
                if keyword_count > 2:
                    table_indicators += 1
            
            doc.close()
            
            has_tables = table_indicators >= 2  # Need at least 2 indicators
            logger.info(f"Table detection for {pdf_path.name}: {'FOUND' if has_tables else 'NOT FOUND'} (indicators: {table_indicators})")
            return has_tables
            
        except Exception as e:
            logger.warning(f"Error during table detection for {pdf_path}: {e}")
            # Default to assuming no tables to avoid crashes
            return False

    def _convert_pdf_with_table_handling(self, pdf_path: Path) -> Tuple[str, Dict[str, Any]]:
        """
        Convert PDF to markdown with intelligent table handling and fallback.
        Includes 9-hour timeout protection to prevent infinite hangs.
        Returns a tuple of (markdown_content, images_dict) or raises an exception if all attempts fail.
        """
        pdf_str = str(pdf_path)
        timeout_seconds = 32400  # 9 hours (was 6 hours = 21600, now increased to 9 hours)

        # Step 1: Detect if tables are present
        has_tables = self._detect_tables(pdf_path)

        if has_tables:
            logger.info(f"Tables detected in {pdf_path.name}, attempting conversion with table recognition (timeout: {timeout_seconds/3600:.1f}h)...")
            try:
                # Attempt conversion with table recognition + TIMEOUT
                with timeout_context(timeout_seconds):
                    result = self.table_converter(pdf_str)

                if result and result.markdown:
                    logger.info(f"Successfully converted {pdf_path.name} with table recognition")
                    images = getattr(result, 'images', {})
                    return result.markdown, images
                else:
                    logger.warning(f"Table conversion returned empty result for {pdf_path.name}")
                    raise ValueError("Empty markdown result from table conversion")

            except TimeoutError as e:
                logger.error(f"Table conversion TIMED OUT after {timeout_seconds}s ({timeout_seconds/3600:.1f}h) for {pdf_path.name}")
                raise e
                    
            except Exception as e:
                # Check if this is a table-related error
                error_str = str(e).lower()
                table_error_indicators = [
                    'table_rec', 'surya', 'torch.stack', 'table_idx', 
                    'tables[', 'row_encoder_hidden_states', 'empty tensor',
                    'size of tensor', 'must match the size', 'non-singleton dimension'
                ]
                
                is_table_error = any(indicator in error_str for indicator in table_error_indicators)
                
                if is_table_error:
                    logger.warning(f"Table recognition failed for {pdf_path.name}: {e}")
                    logger.info(f"Retrying {pdf_path.name} without table recognition (timeout: {timeout_seconds/3600:.1f}h)...")

                    # Fallback: try without table recognition + TIMEOUT
                    try:
                        with timeout_context(timeout_seconds):
                            result = self.no_table_converter(pdf_str)

                        if result and result.markdown:
                            logger.info(f"Successfully converted {pdf_path.name} without table recognition (fallback)")
                            images = getattr(result, 'images', {})
                            return result.markdown, images
                        else:
                            logger.error(f"Fallback conversion also returned empty result for {pdf_path.name}")
                            raise ValueError("Empty markdown result from fallback conversion")

                    except TimeoutError as fallback_timeout:
                        logger.error(f"Fallback conversion TIMED OUT after {timeout_seconds}s ({timeout_seconds/3600:.1f}h) for {pdf_path.name}")
                        raise fallback_timeout

                    except Exception as fallback_error:
                        logger.error(f"Fallback conversion also failed for {pdf_path.name}: {fallback_error}")
                        raise fallback_error
                else:
                    # Non-table related error, re-raise
                    logger.error(f"Non-table error during conversion of {pdf_path.name}: {e}")
                    raise e
        else:
            # No tables detected, use the faster no-table converter + TIMEOUT
            logger.info(f"No tables detected in {pdf_path.name}, using standard conversion (timeout: {timeout_seconds/3600:.1f}h)...")
            try:
                with timeout_context(timeout_seconds):
                    result = self.no_table_converter(pdf_str)

                if result and result.markdown:
                    logger.info(f"Successfully converted {pdf_path.name} without table recognition")
                    images = getattr(result, 'images', {})
                    return result.markdown, images
                else:
                    logger.error(f"Standard conversion returned empty result for {pdf_path.name}")
                    raise ValueError("Empty markdown result from standard conversion")

            except TimeoutError as e:
                logger.error(f"Conversion TIMED OUT after {timeout_seconds}s ({timeout_seconds/3600:.1f}h) for {pdf_path.name}")
                raise e

            except Exception as e:
                logger.error(f"Standard conversion failed for {pdf_path.name}: {e}")
                raise e

    def _extract_header_footer_text(self, pdf_path: Path) -> str:
        """
        Extracts text from the first page, including header and footer,
        using the pymupdf logic from the example. Used for metadata extraction hint.
        """
        try:
            doc = pymupdf.open(pdf_path)
            if not doc or len(doc) == 0:
                print(f"Warning: Could not open or empty PDF: {pdf_path}")
                return ""

            first_page = doc[0]
            page_height = first_page.rect.height
            blocks = first_page.get_text("dict", flags=11)["blocks"]

            header_threshold = 0.1 * page_height
            header_blocks = [b for b in blocks if b["bbox"][3] <= header_threshold]
            header_blocks.sort(key=lambda b: b["bbox"][1])
            header_text_parts = [span["text"].strip() for block in header_blocks for line in block["lines"] for span in line["spans"] if span["text"].strip()]
            header_text = " ".join(header_text_parts)

            footer_threshold = 0.9 * page_height
            footer_blocks = [b for b in blocks if b["bbox"][1] >= footer_threshold]
            footer_blocks.sort(key=lambda b: b["bbox"][1])
            footer_text_parts = [span["text"].strip() for block in footer_blocks for line in block["lines"] for span in line["spans"] if span["text"].strip()]
            footer_text = " ".join(footer_text_parts)

            main_text = first_page.get_text("text")
            doc.close()

            combined_text = f"## Extracted Header:\n{header_text}\n\n## Extracted Footer:\n{footer_text}\n\n## First Page Content:\n{main_text}"
            return combined_text

        except Exception as e:
            print(f"Error extracting header/footer text from {pdf_path}: {e}")
            return ""

    def _save_marker_images(self, doc_id: str, marker_images: Dict[str, Any], image_dir: Path) -> Dict[str, Path]:
        """Save images from Marker's result to organized directory.

        Returns:
            Dict mapping original Marker filenames to new file paths
        """
        from PIL import Image
        import io
        image_mapping = {}  # Maps original_filename -> new_path

        try:
            if not marker_images:
                logger.debug(f"No images to save for doc_id {doc_id}")
                return image_mapping

            # Save each image from the marker images dict
            for idx, (original_filename, image_data) in enumerate(marker_images.items()):
                # Extract extension from original filename
                ext = Path(original_filename).suffix or '.png'
                new_filename = f"image_{idx}{ext}"
                new_path = image_dir / new_filename

                # Handle both PIL Image objects and bytes
                if isinstance(image_data, Image.Image):
                    # Convert PIL Image to bytes
                    img_byte_arr = io.BytesIO()
                    # Determine format from extension, default to PNG
                    save_format = ext.lstrip('.').upper()
                    if save_format == 'JPG':
                        save_format = 'JPEG'
                    image_data.save(img_byte_arr, format=save_format or 'PNG')
                    image_data = img_byte_arr.getvalue()

                # Write image data to file
                with open(new_path, 'wb') as f:
                    f.write(image_data)

                # Store mapping: original Marker filename -> new saved path
                image_mapping[original_filename] = new_path
                logger.debug(f"Saved image: {original_filename} -> {new_path}")

            logger.info(f"Saved {len(image_mapping)} images, mapping: {image_mapping}")
            return image_mapping

        except Exception as e:
            logger.warning(f"Error saving images for doc_id {doc_id}: {e}")
            return image_mapping

    def _update_markdown_image_paths(self, markdown: str, doc_id: str, image_mapping: Dict[str, Path]) -> str:
        """Update image references in markdown to new paths.

        Args:
            markdown: Markdown content with image references from Marker
            doc_id: Document ID
            image_mapping: Dict mapping Marker's original filenames to new Path objects
        """
        import re

        if not image_mapping:
            return markdown

        try:
            # Update markdown image references - Pattern: ![alt](path)
            def replace_image_path(match):
                alt_text = match.group(1)
                old_path = match.group(2)
                old_filename = Path(old_path).name

                # Check if this Marker filename is in our mapping
                if old_filename in image_mapping:
                    new_path = image_mapping[old_filename]
                    new_ref = f"/api/images/{doc_id}/{new_path.name}"
                    logger.info(f"Mapped image: {old_filename} -> {new_path.name}")
                    return f"![{alt_text}]({new_ref})"

                # If not found, keep original
                logger.warning(f"No mapping for: {old_filename}, have: {list(image_mapping.keys())}")
                return match.group(0)

            return re.sub(r'!\[([^\]]*)\]\(([^\)]+)\)', replace_image_path, markdown)

        except Exception as e:
            logger.warning(f"Error updating markdown image paths: {e}")
            return markdown

    def _process_images_for_doc(self, doc_id: str, chunks: List[Dict], image_dir: Path) -> List[Dict]:
        """Extract image metadata from chunks and prepare for embedding."""
        image_data = []

        try:
            # Get all image files in the doc's image directory
            if not image_dir.exists():
                return image_data

            image_files = list(image_dir.glob("*"))
            if not image_files:
                return image_data

            # Create mapping of image paths to chunks that reference them
            for chunk in chunks:
                chunk_id = chunk["metadata"].get("chunk_id")
                image_refs = chunk["metadata"].get("image_refs", [])

                for img_ref in image_refs:
                    img_path = img_ref.get("path", "")
                    alt_text = img_ref.get("alt_text", "")

                    # Find the actual file
                    filename = Path(img_path).name
                    actual_path = image_dir / filename

                    if actual_path.exists():
                        image_id = f"{doc_id}_{filename}"

                        image_data.append({
                            "image_id": image_id,
                            "path": str(actual_path),
                            "chunk_id": chunk_id,
                            "alt_text": alt_text,
                            "metadata": {
                                "doc_id": doc_id,
                                "chunk_id": chunk_id,
                                "position": img_ref.get("position", 0)
                            }
                        })

            logger.info(f"Prepared {len(image_data)} images for embedding from doc {doc_id}")
            return image_data

        except Exception as e:
            logger.warning(f"Error processing images for doc {doc_id}: {e}")
            return image_data

    def _embed_and_store_images(self, doc_id: str, image_data: List[Dict]) -> None:
        """Generate embeddings for images and store in vector store."""
        if not image_data:
            return

        try:
            from ai_researcher import config

            if not config.ENABLE_IMAGE_EMBEDDINGS or not self.vision_embedder:
                logger.debug("Image embeddings disabled, skipping image embedding")
                return

            # Extract image paths
            image_paths = [img["path"] for img in image_data]

            # Generate embeddings
            logger.info(f"Generating embeddings for {len(image_paths)} images...")
            image_embeddings = self.vision_embedder.embed_images(image_paths)

            if not image_embeddings:
                logger.warning(f"Failed to generate image embeddings for doc {doc_id}")
                return

            # Store in vector store
            if self.vector_store and hasattr(self.vector_store, 'add_images'):
                logger.info(f"Storing {len(image_data)} image embeddings in vector store...")
                count = self.vector_store.add_images(doc_id, image_data, image_embeddings)
                logger.info(f"Successfully stored {count} image embeddings for doc {doc_id}")
            else:
                logger.warning("Vector store does not support image storage")

        except Exception as e:
            # Non-fatal error - log and continue
            logger.error(f"Error embedding/storing images for doc {doc_id}: {e}")
            import traceback
            traceback.print_exc()

    def process_pdf(self, pdf_path: Path, doc_id: Optional[str] = None) -> Optional[Dict]:
        """
        Processes a single PDF file.
        Returns a dictionary containing doc_id, markdown content, metadata, and chunks.
        Returns None if processing fails or file is already processed.
        """
        if not pdf_path.exists() or pdf_path.suffix.lower() != ".pdf":
            print(f"Error: Invalid PDF path: {pdf_path}")
            return None

        start_time = time.time()
        print(f"Processing PDF: {pdf_path.name}...")

        # Always process documents - no database checks needed
        # Use provided doc_id or generate a new one
        if doc_id is None:
            doc_id = str(uuid.uuid4())
        final_metadata = {"doc_id": doc_id, "original_filename": pdf_path.name}
        print(f"Processing '{pdf_path.name}' with doc_id: {doc_id}")

        # --- Metadata Extraction (only if not loaded from DB or if forced and failed to load) ---
        # We might want to avoid re-running LLM metadata extraction if force_reembed is just for syncing vector store.
        # Let's refine: only extract if metadata wasn't successfully loaded above.
        if final_metadata is None or not final_metadata.get('title'): # Check if metadata is basic or missing key fields
            print("  Extracting metadata using LLM...")
            initial_text_for_metadata = self._extract_header_footer_text(pdf_path)
            extracted_metadata = self.metadata_extractor.extract(initial_text_for_metadata)
            if extracted_metadata:
                # Ensure doc_id and filename are preserved if they existed
                base_meta = {"doc_id": doc_id, "original_filename": pdf_path.name}
                extracted_metadata.update(base_meta) # Overwrite doc_id/filename from extraction if needed
                final_metadata = extracted_metadata
                print(f"  Successfully extracted metadata for {pdf_path.name}.")
                # Save metadata JSON (overwrite if exists)
                metadata_filename = f"{doc_id}.json"
                metadata_save_path = self.metadata_dir / metadata_filename
                try:
                    with open(metadata_save_path, "w", encoding="utf-8") as f:
                        json.dump(final_metadata, f, indent=2, ensure_ascii=False)
                    print(f"  Saved metadata to: {metadata_save_path}")
                except IOError as e:
                    print(f"  Error saving metadata file {metadata_save_path}: {e}")
            else:
                print(f"  Warning: Metadata extraction failed for {pdf_path.name}. Using basic metadata.")
                # Ensure final_metadata is at least the base if extraction fails
                if final_metadata is None:
                     final_metadata = {"doc_id": doc_id, "original_filename": pdf_path.name}

        # --- Add or Update record in DB ---
        # Database operations now handled by main application database
        # No need to add to separate AI database anymore
        print(f"Added record for '{pdf_path.name}' (ID: {doc_id}) to database.")
        # The metadata extraction logic was already handled correctly in the previous block.
        # --- Get Markdown Content (Convert or Load Existing) ---
        md_filename = f"{doc_id}.md"
        md_save_path = self.markdown_dir / md_filename
        markdown_content = None

        if md_save_path.exists() and self.force_reembed:
            # If forcing re-embed and markdown exists, try loading it
            print(f"  Found existing Markdown file: {md_save_path}. Loading content.")
            try:
                with open(md_save_path, "r", encoding="utf-8") as f:
                    markdown_content = f.read()
                if not markdown_content:
                     print(f"  Warning: Existing Markdown file {md_save_path} is empty. Will attempt Marker conversion.")
                     # Fall through to Marker conversion
            except IOError as e:
                print(f"  Error reading existing Markdown file {md_save_path}: {e}. Will attempt Marker conversion.")
                # Fall through to Marker conversion
            except Exception as e:
                 print(f"  Unexpected error reading existing Markdown file {md_save_path}: {e}. Will attempt Marker conversion.")
                 # Fall through to Marker conversion

        if markdown_content is None:
            # If not loaded (doesn't exist, force_reembed is false, or loading failed), run Marker
            print(f"  Converting PDF to Markdown using Marker with intelligent table handling...")
            try:
                markdown_content, marker_images = self._convert_pdf_with_table_handling(pdf_path)
                if not markdown_content:
                    print(f"Warning: Marker produced empty markdown for {pdf_path.name}. Skipping document.")
                    # Update status? Maybe not, as it might be a valid empty doc. Let chunking handle it.
                    # Let's return None here to be safe, as empty content can't be embedded.
                    print(f"  Warning: Marker returned empty output for {pdf_path.name}")
                    return None
            except Exception as e:
                print(f"  Error converting PDF with Marker for {pdf_path.name}: {e}")
                # Status update removed - handled by caller(doc_id, "error_marker_conversion")
                return None # Fail if marker fails

            # --- Handle extracted images ---
            from ai_researcher import config
            if config.ENABLE_IMAGE_EXTRACTION and marker_images:
                try:
                    print(f"  Processing extracted images...")
                    image_dir = self.image_dir / doc_id
                    image_dir.mkdir(parents=True, exist_ok=True)

                    # Save images from marker output to organized structure
                    image_mapping = self._save_marker_images(doc_id, marker_images, image_dir)

                    if image_mapping:
                        # Update markdown references with proper image mapping
                        markdown_content = self._update_markdown_image_paths(markdown_content, doc_id, image_mapping)
                        print(f"  Organized {len(image_mapping)} images for {pdf_path.name}")
                except Exception as e:
                    print(f"  Warning: Image processing failed for {pdf_path.name}: {e}")
                    # Non-fatal error, continue with document processing

            # Save the newly generated Markdown (with updated image paths if applicable)
            try:
                with open(md_save_path, "w", encoding="utf-8") as f:
                    f.write(markdown_content)
                print(f"  Saved Markdown to: {md_save_path}")
            except IOError as e:
                print(f"  Error saving Markdown file {md_save_path}: {e}")
                # Status update removed - handled by caller(doc_id, "error_saving_markdown")
                return None # Fail if cannot save markdown

        # --- Chunk the Markdown ---
        print(f"  Chunking Markdown content...")
        # Ensure the metadata passed to the chunker *definitely* has the correct filename
        if final_metadata:
             final_metadata['original_filename'] = pdf_path.name
        else:
             # Should not happen at this stage, but handle defensively
             print(f"  Warning: final_metadata is None before chunking for {pdf_path.name}. Using basic.")
             final_metadata = {"doc_id": doc_id, "original_filename": pdf_path.name}

        chunks = self.chunker.chunk(markdown_content, doc_metadata=final_metadata)
        print(f"  Generated {len(chunks)} chunks for {pdf_path.name}.")

        # --- Embed and Store Chunks ---
        chunks_added_count = 0
        if self.embedder and self.vector_store and chunks:
            try:
                print(f"  Embedding {len(chunks)} chunks for {pdf_path.name}...")
                chunks_with_embeddings = self.embedder.embed_chunks(chunks)
                print(f"  Embedding complete. Adding to vector store...")
                # Extract embeddings for vector store
                dense_embeddings = [chunk["embeddings"]["dense"] for chunk in chunks_with_embeddings]
                sparse_embeddings = [chunk["embeddings"]["sparse"] for chunk in chunks_with_embeddings]
                self.vector_store.add_chunks(
                    doc_id=doc_id,
                    chunks=chunks_with_embeddings,
                    dense_embeddings=dense_embeddings,
                    sparse_embeddings=sparse_embeddings,
                    batch_size=50  # Process in batches for better performance
                )
                chunks_added_count = len(chunks)
                print(f"  Successfully added {chunks_added_count} chunks to vector store for {pdf_path.name}.")

                # --- Index in OpenSearch for fulltext search ---
                if config.ENABLE_OPENSEARCH:
                    try:
                        from .opensearch_store import get_opensearch_store
                        os_store = get_opensearch_store()
                        if os_store:
                            os_indexed = os_store.add_chunks(doc_id, chunks_with_embeddings)
                            print(f"  Indexed {os_indexed} chunks in OpenSearch for fulltext search.")
                    except Exception as e_opensearch:
                        print(f"  Warning: OpenSearch indexing failed: {e_opensearch}")

                # --- Build Knowledge Graph ---
                from ai_researcher import config
                if config.ENABLE_KNOWLEDGE_GRAPH:
                    try:
                        print(f"  Building knowledge graph...")
                        from .graph_store import GraphStore
                        graph_store = GraphStore()

                        # Build sequential relationships (always enabled for graph)
                        graph_store.build_sequential_relationships(doc_id, len(chunks))
                        print(f"  Built sequential relationships for {len(chunks)} chunks.")

                        # Extract entities with GLiNER (or spaCy fallback)
                        try:
                            from .entity_extractor import EntityExtractor

                            doc_language = EntityExtractor.detect_language(
                                markdown_content if markdown_content else ""
                            )
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

                            print(f"  Extracted {entities_count} entities ({doc_language}).")
                        except Exception as e_entity:
                            print(f"  Warning: Entity extraction failed: {e_entity}")

                        # Build co-occurrence relationships (always, regardless of LLM setting)
                        cooccurrence_count = graph_store.build_cooccurrence_relationships(
                            doc_id=doc_id,
                            min_cooccurrence=2
                        )
                        print(f"  Built {cooccurrence_count} co-occurrence relationships.")
                    except Exception as e_graph:
                        print(f"  Warning: Knowledge graph building failed: {e_graph}")

                # --- Embed and Store Images ---
                if config.ENABLE_IMAGE_EMBEDDINGS:
                    try:
                        print(f"  Processing image embeddings...")
                        image_dir = self.image_dir / doc_id
                        if image_dir.exists():
                            image_data = self._process_images_for_doc(doc_id, chunks_with_embeddings, image_dir)
                            if image_data:
                                self._embed_and_store_images(doc_id, image_data)
                                print(f"  Successfully processed {len(image_data)} image embeddings for {pdf_path.name}")
                    except Exception as e_image:
                        # Non-fatal error - log and continue
                        print(f"  Warning: Image embedding failed for {pdf_path.name}: {e_image}")

            except Exception as e_embed_store:
                print(f"Error embedding/storing chunks for {pdf_path.name}: {e_embed_store}")
                # Decide if this should be a fatal error for the document or just a warning
                # For now, log the error and continue, but don't return the document data
                # as it wasn't fully processed into the vector store.
                # Alternatively, could return partial data or raise exception.
                # Let's return None to indicate failure at this stage.
                # Status update removed - handled by caller(doc_id, "error_embedding_storing")
                return None
        elif not chunks:
             print(f"  Skipping embedding/storing for {pdf_path.name}: No chunks generated.")
        else:
             print(f"  Skipping embedding/storing for {pdf_path.name}: Embedder or VectorStore not provided.")
        # --- End Embed and Store ---


        end_time = time.time()
        print(f"Finished processing {pdf_path.name} in {end_time - start_time:.2f} seconds (Added {chunks_added_count} chunks to vector store).")

        # Return minimal data now, as chunks are handled internally
        return {
            "doc_id": doc_id,
            "original_filename": pdf_path.name,
            "chunks_generated": len(chunks), # Keep track of generated chunks
            "chunks_added_to_vector_store": chunks_added_count, # Keep track of added chunks
            "extracted_metadata": final_metadata # Return metadata for potential logging/summary
            # Removed markdown_path and markdown_content as they are less relevant for the summary return
        }

    def process_word_document(self, word_path: Path, doc_id: Optional[str] = None) -> Optional[Dict]:
        """
        Processes a single Word document.
        Returns a dictionary containing doc_id, markdown content, metadata, and chunks.
        Returns None if processing fails or file is already processed.
        """
        if not word_path.exists() or not self.document_converter.is_word_document(word_path.name):
            print(f"Error: Invalid Word document path: {word_path}")
            return None

        start_time = time.time()
        print(f"Processing Word document: {word_path.name}...")

        # Always process documents - no database checks needed
        # Use provided doc_id or generate a new one
        if doc_id is None:
            doc_id = str(uuid.uuid4())
        final_metadata = {"doc_id": doc_id, "original_filename": word_path.name}
        print(f"Processing '{word_path.name}' with doc_id: {doc_id}")
        
        # Skip the old database check logic
        if False:
            print(f"Skipping '{word_path.name}': Already processed and found in database (use --force-reembed to override).")
            return None
        elif existing_doc_info and self.force_reembed:
            print(f"Force re-embedding '{word_path.name}': Found existing record in database.")
            doc_id = existing_doc_info['doc_id']
            try:
                final_metadata = json.loads(existing_doc_info['metadata_json']) if existing_doc_info['metadata_json'] else {}
                final_metadata['doc_id'] = doc_id
                final_metadata['original_filename'] = word_path.name
                print(f"  Using existing doc_id: {doc_id} and loaded metadata.")
            except json.JSONDecodeError:
                print(f"  Warning: Could not parse existing metadata for {doc_id}. Using basic metadata.")
                final_metadata = {"doc_id": doc_id, "original_filename": word_path.name}
        else:
            print(f"Processing '{word_path.name}' as a new document.")
            doc_id = str(uuid.uuid4())
            final_metadata = {"doc_id": doc_id, "original_filename": word_path.name}

        # --- Metadata Extraction ---
        if final_metadata is None or not final_metadata.get('title'):
            print("  Extracting metadata using LLM...")
            initial_text_for_metadata = self.document_converter.extract_initial_text_for_metadata(word_path)
            extracted_metadata = self.metadata_extractor.extract(initial_text_for_metadata)
            if extracted_metadata:
                base_meta = {"doc_id": doc_id, "original_filename": word_path.name}
                extracted_metadata.update(base_meta)
                final_metadata = extracted_metadata
                print(f"  Successfully extracted metadata for {word_path.name}.")
                # Save metadata JSON
                metadata_filename = f"{doc_id}.json"
                metadata_save_path = self.metadata_dir / metadata_filename
                try:
                    with open(metadata_save_path, "w", encoding="utf-8") as f:
                        json.dump(final_metadata, f, indent=2, ensure_ascii=False)
                    print(f"  Saved metadata to: {metadata_save_path}")
                except IOError as e:
                    print(f"  Error saving metadata file {metadata_save_path}: {e}")
            else:
                print(f"  Warning: Metadata extraction failed for {word_path.name}. Using basic metadata.")
                if final_metadata is None:
                    final_metadata = {"doc_id": doc_id, "original_filename": word_path.name}

        # --- Add or Update record in DB ---
        # Database operations now handled by main application database
        # No need to add to separate AI database anymore
        print(f"Added record for '{word_path.name}' (ID: {doc_id}) to database.")

        # --- Convert Word to Markdown ---
        md_filename = f"{doc_id}.md"
        md_save_path = self.markdown_dir / md_filename
        markdown_content = None

        if md_save_path.exists() and self.force_reembed:
            print(f"  Found existing Markdown file: {md_save_path}. Loading content.")
            try:
                with open(md_save_path, "r", encoding="utf-8") as f:
                    markdown_content = f.read()
                if not markdown_content:
                    print(f"  Warning: Existing Markdown file {md_save_path} is empty. Will convert from Word.")
            except IOError as e:
                print(f"  Error reading existing Markdown file {md_save_path}: {e}. Will convert from Word.")

        if markdown_content is None:
            print(f"  Converting Word document to Markdown...")
            try:
                markdown_content = self.document_converter.convert_word_to_markdown(word_path)
                if not markdown_content:
                    print(f"Warning: Word conversion produced empty markdown for {word_path.name}. Skipping document.")
                    # Status update removed - handled by caller(doc_id, "error_word_empty_output")
                    return None
            except Exception as e:
                print(f"  Error converting Word document for {word_path.name}: {e}")
                # Status update removed - handled by caller(doc_id, "error_word_conversion")
                return None

            # Save the newly generated Markdown
            try:
                with open(md_save_path, "w", encoding="utf-8") as f:
                    f.write(markdown_content)
                print(f"  Saved Markdown to: {md_save_path}")
            except IOError as e:
                print(f"  Error saving Markdown file {md_save_path}: {e}")
                # Status update removed - handled by caller(doc_id, "error_saving_markdown")
                return None

        # --- Chunk the Markdown ---
        print(f"  Chunking Markdown content...")
        if final_metadata:
            final_metadata['original_filename'] = word_path.name
        else:
            final_metadata = {"doc_id": doc_id, "original_filename": word_path.name}

        chunks = self.chunker.chunk(markdown_content, doc_metadata=final_metadata)
        print(f"  Generated {len(chunks)} chunks for {word_path.name}.")

        # --- Embed and Store Chunks ---
        chunks_added_count = 0
        if self.embedder and self.vector_store and chunks:
            try:
                print(f"  Embedding {len(chunks)} chunks for {word_path.name}...")
                chunks_with_embeddings = self.embedder.embed_chunks(chunks)
                print(f"  Embedding complete. Adding to vector store...")
                # Extract embeddings for vector store
                dense_embeddings = [chunk["embeddings"]["dense"] for chunk in chunks_with_embeddings]
                sparse_embeddings = [chunk["embeddings"]["sparse"] for chunk in chunks_with_embeddings]
                self.vector_store.add_chunks(
                    doc_id=doc_id,
                    chunks=chunks_with_embeddings,
                    dense_embeddings=dense_embeddings,
                    sparse_embeddings=sparse_embeddings,
                    batch_size=50  # Process in batches for better performance
                )
                chunks_added_count = len(chunks)
                print(f"  Successfully added {chunks_added_count} chunks to vector store for {word_path.name}.")
            except Exception as e_embed_store:
                print(f"Error embedding/storing chunks for {word_path.name}: {e_embed_store}")
                # Status update removed - handled by caller(doc_id, "error_embedding_storing")
                return None
        elif not chunks:
            print(f"  Skipping embedding/storing for {word_path.name}: No chunks generated.")
        else:
            print(f"  Skipping embedding/storing for {word_path.name}: Embedder or VectorStore not provided.")

        end_time = time.time()
        print(f"Finished processing {word_path.name} in {end_time - start_time:.2f} seconds (Added {chunks_added_count} chunks to vector store).")

        return {
            "doc_id": doc_id,
            "original_filename": word_path.name,
            "chunks_generated": len(chunks),
            "chunks_added_to_vector_store": chunks_added_count,
            "extracted_metadata": final_metadata
        }

    def process_markdown_file(self, markdown_path: Path, doc_id: Optional[str] = None) -> Optional[Dict]:
        """
        Processes a single Markdown file.
        Returns a dictionary containing doc_id, markdown content, metadata, and chunks.
        Returns None if processing fails or file is already processed.
        """
        if not markdown_path.exists() or not self.document_converter.is_markdown_file(markdown_path.name):
            print(f"Error: Invalid Markdown file path: {markdown_path}")
            return None

        start_time = time.time()
        print(f"Processing Markdown file: {markdown_path.name}...")

        # Always process documents - no database checks needed
        # Use provided doc_id or generate a new one
        if doc_id is None:
            doc_id = str(uuid.uuid4())
        final_metadata = {"doc_id": doc_id, "original_filename": markdown_path.name}
        print(f"Processing '{markdown_path.name}' with doc_id: {doc_id}")
        
        # Skip the old database check logic
        if False:
            print(f"Skipping '{markdown_path.name}': Already processed and found in database (use --force-reembed to override).")
            return None
        elif existing_doc_info and self.force_reembed:
            print(f"Force re-embedding '{markdown_path.name}': Found existing record in database.")
            doc_id = existing_doc_info['doc_id']
            try:
                final_metadata = json.loads(existing_doc_info['metadata_json']) if existing_doc_info['metadata_json'] else {}
                final_metadata['doc_id'] = doc_id
                final_metadata['original_filename'] = markdown_path.name
                print(f"  Using existing doc_id: {doc_id} and loaded metadata.")
            except json.JSONDecodeError:
                print(f"  Warning: Could not parse existing metadata for {doc_id}. Using basic metadata.")
                final_metadata = {"doc_id": doc_id, "original_filename": markdown_path.name}
        else:
            print(f"Processing '{markdown_path.name}' as a new document.")
            doc_id = str(uuid.uuid4())
            final_metadata = {"doc_id": doc_id, "original_filename": markdown_path.name}

        # --- Metadata Extraction ---
        if final_metadata is None or not final_metadata.get('title'):
            print("  Extracting metadata using LLM...")
            initial_text_for_metadata = self.document_converter.extract_initial_text_for_metadata(markdown_path)
            extracted_metadata = self.metadata_extractor.extract(initial_text_for_metadata)
            if extracted_metadata:
                base_meta = {"doc_id": doc_id, "original_filename": markdown_path.name}
                extracted_metadata.update(base_meta)
                final_metadata = extracted_metadata
                print(f"  Successfully extracted metadata for {markdown_path.name}.")
                # Save metadata JSON
                metadata_filename = f"{doc_id}.json"
                metadata_save_path = self.metadata_dir / metadata_filename
                try:
                    with open(metadata_save_path, "w", encoding="utf-8") as f:
                        json.dump(final_metadata, f, indent=2, ensure_ascii=False)
                    print(f"  Saved metadata to: {metadata_save_path}")
                except IOError as e:
                    print(f"  Error saving metadata file {metadata_save_path}: {e}")
            else:
                print(f"  Warning: Metadata extraction failed for {markdown_path.name}. Using basic metadata.")
                if final_metadata is None:
                    final_metadata = {"doc_id": doc_id, "original_filename": markdown_path.name}

        # --- Add or Update record in DB ---
        # Database operations now handled by main application database
        # No need to add to separate AI database anymore
        print(f"Added record for '{markdown_path.name}' (ID: {doc_id}) to database.")

        # --- Read Markdown Content ---
        md_filename = f"{doc_id}.md"
        md_save_path = self.markdown_dir / md_filename
        markdown_content = None

        if md_save_path.exists() and self.force_reembed:
            print(f"  Found existing processed Markdown file: {md_save_path}. Loading content.")
            try:
                with open(md_save_path, "r", encoding="utf-8") as f:
                    markdown_content = f.read()
                if not markdown_content:
                    print(f"  Warning: Existing processed Markdown file {md_save_path} is empty. Will read from original.")
            except IOError as e:
                print(f"  Error reading existing processed Markdown file {md_save_path}: {e}. Will read from original.")

        if markdown_content is None:
            print(f"  Reading Markdown file content...")
            try:
                markdown_content = self.document_converter.read_markdown_file(markdown_path)
                if not markdown_content:
                    print(f"Warning: Markdown file is empty for {markdown_path.name}. Skipping document.")
                    # Status update removed - handled by caller(doc_id, "error_markdown_empty")
                    return None
            except Exception as e:
                print(f"  Error reading Markdown file {markdown_path.name}: {e}")
                # Status update removed - handled by caller(doc_id, "error_markdown_reading")
                return None

            # Save a copy of the processed Markdown (for consistency with other formats)
            try:
                with open(md_save_path, "w", encoding="utf-8") as f:
                    f.write(markdown_content)
                print(f"  Saved processed Markdown to: {md_save_path}")
            except IOError as e:
                print(f"  Error saving processed Markdown file {md_save_path}: {e}")
                # Status update removed - handled by caller(doc_id, "error_saving_markdown")
                return None

        # --- Chunk the Markdown ---
        print(f"  Chunking Markdown content...")
        if final_metadata:
            final_metadata['original_filename'] = markdown_path.name
        else:
            final_metadata = {"doc_id": doc_id, "original_filename": markdown_path.name}

        chunks = self.chunker.chunk(markdown_content, doc_metadata=final_metadata)
        print(f"  Generated {len(chunks)} chunks for {markdown_path.name}.")

        # --- Embed and Store Chunks ---
        chunks_added_count = 0
        if self.embedder and self.vector_store and chunks:
            try:
                print(f"  Embedding {len(chunks)} chunks for {markdown_path.name}...")
                chunks_with_embeddings = self.embedder.embed_chunks(chunks)
                print(f"  Embedding complete. Adding to vector store...")
                # Extract embeddings for vector store
                dense_embeddings = [chunk["embeddings"]["dense"] for chunk in chunks_with_embeddings]
                sparse_embeddings = [chunk["embeddings"]["sparse"] for chunk in chunks_with_embeddings]
                self.vector_store.add_chunks(
                    doc_id=doc_id,
                    chunks=chunks_with_embeddings,
                    dense_embeddings=dense_embeddings,
                    sparse_embeddings=sparse_embeddings,
                    batch_size=50  # Process in batches for better performance
                )
                chunks_added_count = len(chunks)
                print(f"  Successfully added {chunks_added_count} chunks to vector store for {markdown_path.name}.")
            except Exception as e_embed_store:
                print(f"Error embedding/storing chunks for {markdown_path.name}: {e_embed_store}")
                # Status update removed - handled by caller(doc_id, "error_embedding_storing")
                return None
        elif not chunks:
            print(f"  Skipping embedding/storing for {markdown_path.name}: No chunks generated.")
        else:
            print(f"  Skipping embedding/storing for {markdown_path.name}: Embedder or VectorStore not provided.")

        end_time = time.time()
        print(f"Finished processing {markdown_path.name} in {end_time - start_time:.2f} seconds (Added {chunks_added_count} chunks to vector store).")

        return {
            "doc_id": doc_id,
            "original_filename": markdown_path.name,
            "chunks_generated": len(chunks),
            "chunks_added_to_vector_store": chunks_added_count,
            "extracted_metadata": final_metadata
        }

    def process_epub(self, epub_path: Path, doc_id: Optional[str] = None) -> Optional[Dict]:
        """
        Processes a single EPUB ebook (batch/CLI path).

        Converts to Markdown via the epub_worker subprocess — the same engine
        the upload path (background_document_processor) uses — rewrites image
        references to ``/api/images/<doc_id>/``, then delegates chunking /
        embedding / metadata to ``process_markdown_file``. The upload path is
        the primary entry point; this method gives the CLI/batch path parity.
        """
        if not epub_path.exists() or not self.document_converter.is_epub_file(epub_path.name):
            print(f"Error: Invalid EPUB file path: {epub_path}")
            return None

        if doc_id is None:
            doc_id = str(uuid.uuid4())

        md_save_path = self.markdown_dir / f"{doc_id}.md"
        image_dir = self.image_dir / doc_id

        print(f"Converting EPUB '{epub_path.name}' via epub_worker...")
        markdown_content, image_mapping = self._run_epub_worker(
            doc_id, epub_path, md_save_path, image_dir
        )
        if not markdown_content:
            print(f"Error: epub_worker produced empty markdown for {epub_path.name}")
            return None

        if image_mapping:
            mapping_as_paths = {orig: image_dir / new for orig, new in image_mapping.items()}
            markdown_content = self._update_markdown_image_paths(markdown_content, doc_id, mapping_as_paths)
            md_save_path.write_text(markdown_content, encoding="utf-8")

        # Delegate the rest (chunking, embedding, metadata) to the markdown
        # pipeline — the converted .md is a first-class markdown document.
        return self.process_markdown_file(md_save_path, doc_id=doc_id)

    def _run_epub_worker(
        self, doc_id: str, epub_path: Path, out_md_path: Path, out_images_dir: Path
    ) -> Tuple[str, Dict[str, str]]:
        """Invoke ``ai_researcher.epub_worker`` and parse its JSON result.

        Returns ``(markdown, image_mapping)``. Raises RuntimeError on any
        failure (non-zero exit, missing result line, or ``ok: false``).
        Mirrors background_document_processor._convert_epub_via_subprocess;
        kept separate because the two processors are independent modules.
        """
        import os as _os
        import subprocess
        import sys as _sys
        out_md_path.parent.mkdir(parents=True, exist_ok=True)
        cmd = [
            _sys.executable, "-m", "ai_researcher.epub_worker",
            str(epub_path), str(out_md_path), str(out_images_dir),
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True, env=_os.environ.copy())
        if proc.returncode != 0:
            raise RuntimeError(
                f"epub_worker exited with code {proc.returncode}: "
                f"{(proc.stderr or '').strip()[-400:]}"
            )
        last_line = ""
        for line in (proc.stdout or "").splitlines()[::-1]:
            line = line.strip()
            if line.startswith("{") and line.endswith("}"):
                last_line = line
                break
        if not last_line:
            raise RuntimeError("epub_worker produced no JSON result")
        result = json.loads(last_line)
        if not result.get("ok"):
            raise RuntimeError(f"epub_worker reported failure: {result.get('error')}")
        return out_md_path.read_text(encoding="utf-8"), result.get("image_mapping") or {}

    def process_document(self, file_path: Path, doc_id: Optional[str] = None) -> Optional[Dict]:
        """
        Generic method to process any supported document format.
        Automatically detects file type and routes to appropriate processing method.
        """
        if not file_path.exists():
            print(f"Error: File does not exist: {file_path}")
            return None
            
        filename = file_path.name
        
        if filename.lower().endswith('.pdf'):
            return self.process_pdf(file_path, doc_id=doc_id)
        elif self.document_converter.is_word_document(filename):
            return self.process_word_document(file_path, doc_id=doc_id)
        elif self.document_converter.is_markdown_file(filename):
            return self.process_markdown_file(file_path, doc_id=doc_id)
        elif self.document_converter.is_epub_file(filename):
            return self.process_epub(file_path, doc_id=doc_id)
        else:
            print(f"Error: Unsupported file format: {filename}")
            return None

    def process_directory(self, process_path: Optional[Path] = None) -> Tuple[int, int]:
        """
        Processes all supported document files in the configured directory or specified path.
        Supports PDF, Word (docx, doc), and Markdown (md, markdown) files.
        Embeds and stores chunks for each document immediately after processing.
        Returns a tuple: (total_documents_processed, total_chunks_added).
        """
        if process_path is None:
            process_path = self.pdf_dir
            
        total_docs_attempted = 0
        total_docs_successfully_processed = 0
        total_chunks_added = 0
        
        # Find all supported file types
        supported_extensions = ['*.pdf', '*.docx', '*.doc', '*.md', '*.markdown', '*.epub']
        all_files = []

        for extension in supported_extensions:
            files = list(process_path.glob(extension))
            all_files.extend(files)

        total_docs_attempted = len(all_files)
        print(f"Found {total_docs_attempted} supported document(s) in {process_path}")

        # Group files by type for reporting
        pdf_files = [f for f in all_files if f.suffix.lower() == '.pdf']
        word_files = [f for f in all_files if f.suffix.lower() in ['.docx', '.doc']]
        markdown_files = [f for f in all_files if f.suffix.lower() in ['.md', '.markdown']]
        epub_files = [f for f in all_files if f.suffix.lower() == '.epub']

        print(f"  - {len(pdf_files)} PDF file(s)")
        print(f"  - {len(word_files)} Word document(s)")
        print(f"  - {len(markdown_files)} Markdown file(s)")
        print(f"  - {len(epub_files)} EPUB file(s)")

        if not all_files:
            print("No supported document files found to process.")
            return 0, 0

        for file_path in all_files:
            result = self.process_document(file_path)  # Generic method handles all file types
            if result:
                # result is not None, meaning processing was successful for this doc
                total_docs_successfully_processed += 1
                total_chunks_added += result.get("chunks_added_to_vector_store", 0)
            # If result is None, an error occurred or it was skipped (and not forced)

        print(f"\nFinished processing directory.")
        print(f"Attempted to process: {total_docs_attempted} document(s).")
        print(f"Successfully processed/re-processed: {total_docs_successfully_processed} document(s).")
        print(f"Total chunks added/updated in vector store during this run: {total_chunks_added}.")
        # Return the count of successfully processed docs and chunks added in this run
        return total_docs_successfully_processed, total_chunks_added
