"""PDF -> Markdown conversion core: page labels + the Marker path.

Trimmed vendor move (issue #118): only what the runner's pdf_worker
subprocess uses — ``extract_page_labels`` (pymupdf page-label map for
locators) and ``DocumentProcessor``'s Marker conversion with table
detection + fallback. The old kitchen-sink (word/markdown/epub entry
points, vector stores, image embedding, LLM OCR config) stayed behind —
the runner shells epub_worker directly and never called the rest.
"""
import os
import logging
import signal
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

import re
import pymupdf  # PyMuPDF (page labels + table detection)

# NOTE: torch and marker are NOT imported at module level. They are
# lazy-loaded inside __init__ (load_marker branch) so that importing this
# module doesn't pull torch into the process (issue #14) — the runner's
# long-lived process only pays the CUDA cost when a real conversion runs.



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
    from axiom_ng_runner.chunking import safe_page_label

    for i in range(n):
        label = safe_page_label(doc[i])
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
    """Marker-based PDF -> Markdown conversion with table detection + fallback."""


    def __init__(
        self,
        pdf_dir: str | Path = "data/raw_pdfs",
        markdown_dir: str | Path = "data/processed/markdown",
        metadata_dir: str | Path = "data/processed/metadata",
        db_path: Optional[str | Path] = None,
        embedder=None,
        vector_store=None,
        force_reembed: bool = False,
        device: Optional[str] = None,
        load_marker: bool = True,
    ):
        # Signature preserved for the pdf_worker subprocess caller; only the
        # Marker conversion path survives the vendor move (#118). Dirs/db/
        # embedder args are accepted and ignored.
        self.pdf_dir = Path(pdf_dir)
        self.markdown_dir = Path(markdown_dir)

        # Device selection and hardware detection are only needed when
        # loading Marker (which uses torch). When load_marker=False, skip
        # entirely — no torch import.
        if load_marker:
            from axiom_ng_runner.compute_core.devices import hardware_detector
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


    def _init_marker_configs(self):
        """Initialize the marker converter pair for table handling.

        LLM-assisted OCR config stayed behind with the vendor move — the
        runner never ran it (MARKER_USE_LLM default false in production).
        """
        from marker.converters.pdf import PdfConverter
        from marker.config.parser import ConfigParser

        # Reduce batch multiplier to avoid overwhelming concurrent vision requests.
        batch_multiplier = 1

        extract_images = os.getenv("ENABLE_IMAGE_EXTRACTION", "True").lower() == "true"
        base_options = {
            "output_format": "markdown",
            "device": self.device,
            "batch_multiplier": batch_multiplier,
            "disable_image_extraction": not extract_images,
            "disable_image_captions": True,  # Skip LLM captions (using CLIP embeddings instead)
            "paginate_output": True,  # Inject page markers for citation page numbers
        }


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

