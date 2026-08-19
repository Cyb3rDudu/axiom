import re
import os
import logging
from typing import List, Dict, Any, Optional

logger = logging.getLogger(__name__)

# Marker page marker pattern: {0}------------------------------------------------
_PAGE_MARKER_RE = re.compile(r'^\{(\d+)\}-{10,}$')


def _slice_page_bounds(parent_text: str, subs: list, parent_bounds: list) -> list:
    """#194: per-sub-chunk page bounds from the parent's map.

    Locates each sub-slice inside the parent text (slices may overlap;
    the search cursor advances past each hit's start), shifts the
    parent boundaries that fall inside the slice, and guarantees a
    leading (0, page) entry — the page in force at the slice start.
    """
    out: list = []
    cursor = 0
    for sub in subs:
        pos = parent_text.find(sub, cursor)
        if pos < 0:
            pos = cursor  # degraded trust in findability; offsets stay monotone
        end = pos + len(sub)
        bounds = [[str(off - pos), str(lab)] for off, lab in parent_bounds if pos < off < end]
        page_at_start = "1"
        for off, lab in parent_bounds:
            if off <= pos:
                page_at_start = str(lab)
        out.append([("0", page_at_start)] + bounds)
        cursor = pos + 1
    return out


class Chunker:
    """
    Splits Markdown text into overlapping chunks based on section boundaries and token budget.

    Features:
    - Structure-aware chunking respecting section boundaries
    - Token budget instead of fixed paragraph count
    - Hierarchical title extraction for embedding context
    - Recursive semantic splitting for oversized chunks
    """
    def __init__(
        self,
        max_chunk_tokens: int = 512,
        overlap_tokens: int = 64,
        min_chunk_tokens: int = 50,
        # Legacy support
        paragraphs_per_chunk: int = 2,
        overlap_paragraphs: int = 1
    ):
        # Determine chunking mode
        self._use_token_chunking = os.getenv("CHUNKER_MODE", "token").lower() != "paragraph"

        if self._use_token_chunking:
            self.max_chunk_tokens = max_chunk_tokens
            self.overlap_tokens = overlap_tokens
            self.min_chunk_tokens = min_chunk_tokens
            logger_msg = f"Token-based chunking: max={max_chunk_tokens}, overlap={overlap_tokens}, min={min_chunk_tokens}"
        else:
            # Legacy paragraph-based mode
            if overlap_paragraphs >= paragraphs_per_chunk:
                raise ValueError("Overlap paragraphs must be less than paragraphs per chunk.")
            self.paragraphs_per_chunk = paragraphs_per_chunk
            self.overlap_paragraphs = overlap_paragraphs
            logger_msg = f"Paragraph-based chunking: {paragraphs_per_chunk} per chunk, {overlap_paragraphs} overlap"

        # Use regex to split by one or more newlines, keeping separators
        self._paragraph_split_pattern = re.compile(r'(\n\s*\n+)')
        # Pattern to detect markdown headings
        self._heading_pattern = re.compile(r'^(#{1,6})\s+(.+)$', re.MULTILINE)
        # Pattern to detect markdown images
        self._image_pattern = re.compile(r'!\[([^\]]*)\]\(([^\)]+)\)')

        logger.info(f"Chunker initialized: {logger_msg}")

    def _count_tokens(self, text: str) -> int:
        """Approximate token count using word count * 1.3 ratio."""
        # Simple approximation: words * 1.3 for English text
        # This is faster than tiktoken and good enough for chunking
        return int(len(text.split()) * 1.3)

    def _extract_images_from_text(self, text: str) -> List[Dict[str, Any]]:
        """Extract image references from markdown text."""
        images = []
        for match in self._image_pattern.finditer(text):
            images.append({
                "alt_text": match.group(1),
                "path": match.group(2),
                "position": match.start()
            })
        return images

    def _extract_heading_hierarchy(self, markdown_content: str) -> Dict[int, List[str]]:
        """
        Extract heading hierarchy for each paragraph position.

        Returns:
            Dict mapping paragraph_index -> List[ancestor headings]
        """
        # Split into paragraphs
        parts = self._paragraph_split_pattern.split(markdown_content)
        paragraphs = []
        current_paragraph = ""

        for part in parts:
            if part:
                if self._paragraph_split_pattern.match(part):
                    if current_paragraph:
                        current_paragraph += part
                        paragraphs.append(current_paragraph.strip())
                    current_paragraph = ""
                else:
                    current_paragraph += part
        if current_paragraph.strip():
            paragraphs.append(current_paragraph.strip())

        # Track heading hierarchy
        heading_stack = []  # List of (level, title)
        paragraph_to_headings = {}

        for i, para in enumerate(paragraphs):
            lines = para.strip().split('\n')
            first_line = lines[0].strip() if lines else ""

            # Check if this paragraph starts with a heading
            heading_match = self._heading_pattern.match(first_line)
            if heading_match:
                level = len(heading_match.group(1))
                title = heading_match.group(2).strip()

                # Pop lower-level headings from stack
                while heading_stack and heading_stack[-1][0] >= level:
                    heading_stack.pop()

                # Add this heading to stack
                heading_stack.append((level, title))

            # Store current hierarchy for this paragraph
            paragraph_to_headings[i] = [h[1] for h in heading_stack]

        return paragraph_to_headings

    def _recursive_split(self, text: str, max_tokens: int, overlap_tokens: int) -> List[str]:
        """
        Recursively split oversized text using semantic boundaries.

        Separators hierarchy:
        1. \n\n (paragraph break)
        2. \n (line break)
        3. . (sentence end)
        4. ; or , (clause break)
        5. Binary split at midpoint
        """
        if self._count_tokens(text) <= max_tokens:
            return [text]

        separators = [
            ("\n\n", 2),   # Paragraph break
            ("\n", 1),      # Line break
            (". ", 2),      # Sentence end
            ("; ", 2),      # Clause break
            (", ", 2),      # Clause break
        ]

        for sep, sep_tokens in separators:
            if sep in text:
                parts = text.split(sep)
                if len(parts) > 1:
                    # Find split point closest to midpoint within budget
                    mid_idx = len(parts) // 2

                    # Try to find a good split point
                    left_text = ""
                    right_text = ""

                    for i in range(mid_idx, 0, -1):
                        candidate = sep.join(parts[:i])
                        if self._count_tokens(candidate) <= max_tokens:
                            left_text = candidate
                            right_text = sep.join(parts[i:])
                            break

                    if not left_text:
                        # No good split found, try from beginning
                        for i in range(1, len(parts)):
                            candidate = sep.join(parts[:i])
                            if self._count_tokens(candidate) > max_tokens:
                                left_text = sep.join(parts[:i-1]) if i > 1 else parts[0]
                                right_text = sep.join(parts[max(1, i-1):])
                                break

                    if left_text and right_text:
                        # Add overlap to right chunk
                        if overlap_tokens > 0:
                            left_words = left_text.split()
                            overlap_words = left_words[-min(len(left_words), overlap_tokens):]
                            right_text = " ".join(overlap_words) + " " + right_text

                        # Recursively split if still too large
                        result = []
                        for chunk in self._recursive_split(left_text, max_tokens, overlap_tokens):
                            result.append(chunk)
                        for chunk in self._recursive_split(right_text, max_tokens, overlap_tokens):
                            result.append(chunk)
                        return result

        # Fallback: binary split at word midpoint
        words = text.split()
        if len(words) > max_tokens:
            mid = len(words) // 2
            left = " ".join(words[:mid])
            right = " ".join(words[mid - overlap_tokens:] if overlap_tokens > 0 else words[mid:])

            result = []
            for chunk in self._recursive_split(left, max_tokens, overlap_tokens):
                result.append(chunk)
            for chunk in self._recursive_split(right, max_tokens, overlap_tokens):
                result.append(chunk)
            return result

        return [text]

    def chunk(self, markdown_content: str, doc_metadata: Optional[Dict[str, Any]] = None) -> List[Dict[str, Any]]:
        """
        Chunks the provided Markdown content using structure-aware token-based chunking.

        Args:
            markdown_content: The full Markdown text of the document.
            doc_metadata: Optional dictionary containing document-level metadata
                          (e.g., doc_id) to be added to each chunk's metadata.

        Returns:
            A list of chunk dictionaries, where each dictionary contains
            the chunk 'text' and its 'metadata'.
        """
        if not markdown_content:
            return []

        # Extract heading hierarchy for all paragraphs
        paragraph_to_headings = self._extract_heading_hierarchy(markdown_content)

        # Split into paragraphs
        parts = self._paragraph_split_pattern.split(markdown_content)
        paragraphs = []
        current_paragraph = ""

        for part in parts:
            if part:
                if self._paragraph_split_pattern.match(part):
                    if current_paragraph:
                        current_paragraph += part
                        paragraphs.append(current_paragraph.strip())
                    current_paragraph = ""
                else:
                    current_paragraph += part
        if current_paragraph.strip():
            paragraphs.append(current_paragraph.strip())

        if not paragraphs:
            return []

        # Extract page numbers from Marker pagination markers and build
        # paragraph_index -> page_label mapping. Strip markers from content.
        # Uses page_label_map from doc_metadata to convert physical index → logical label.
        page_label_map = doc_metadata.get("page_label_map", {}) if doc_metadata else {}
        paragraph_to_page = {}
        paragraph_to_phys: dict[int, int] = {}  # W12: physical marker index per para
        current_page = "1"
        current_phys: int | None = None
        clean_paragraphs = []
        for i, para in enumerate(paragraphs):
            page_match = _PAGE_MARKER_RE.match(para.strip())
            if page_match:
                physical_idx = int(page_match.group(1))
                # Map to logical label, fallback to physical + 1
                current_page = page_label_map.get(physical_idx, str(physical_idx + 1))
                current_phys = physical_idx
                continue  # Skip the marker paragraph itself
            paragraph_to_page[len(clean_paragraphs)] = current_page
            if current_phys is not None:
                paragraph_to_phys[len(clean_paragraphs)] = current_phys
            clean_paragraphs.append(para)

        if not clean_paragraphs:
            return []

        # Re-extract heading hierarchy for clean paragraphs if we removed markers
        if len(clean_paragraphs) != len(paragraphs):
            clean_content = "\n\n".join(clean_paragraphs)
            paragraph_to_headings = self._extract_heading_hierarchy(clean_content)
            paragraphs = clean_paragraphs

        # Choose chunking mode
        if self._use_token_chunking:
            return self._chunk_token_based(paragraphs, paragraph_to_headings, doc_metadata, paragraph_to_page, paragraph_to_phys)
        else:
            return self._chunk_paragraph_based(paragraphs, doc_metadata)

    def _chunk_token_based(
        self,
        paragraphs: List[str],
        paragraph_to_headings: Dict[int, List[str]],
        doc_metadata: Optional[Dict[str, Any]],
        paragraph_to_page: Optional[Dict[int, int]] = None,
        paragraph_to_phys: Optional[Dict[int, int]] = None,
    ) -> List[Dict[str, Any]]:
        """Token-based structure-aware chunking with page number tracking."""
        chunks = []
        doc_id = doc_metadata.get("doc_id", "unknown_doc") if doc_metadata else "unknown_doc"
        chunk_id_counter = 0
        paragraph_to_page = paragraph_to_page or {}
        paragraph_to_phys = paragraph_to_phys or {}
        # W12: corroborated chapter-relative books carry a physical→ordinal
        # map (page_trust.chapter_restarts); the chunk stamps the ordinal of
        # the page its FIRST content sits on (snapshot-at-open, like the
        # section trail #186). Keys may arrive as strings (JSON) — coerce.
        raw_chmap = doc_metadata.get("page_chapter_map") if doc_metadata else None
        chapter_map = {int(k): v for k, v in (raw_chmap or {}).items()}

        current_chunk_paras = []
        current_chunk_tokens = 0
        current_start_idx = 0
        # #194 per-paragraph page map: char-offset boundaries where the
        # chunk's print page CHANGES — [(offset, label), ...], first entry
        # always (0, first page). Lets any consumer derive the exact page
        # of a hit position instead of citing the whole span.
        current_chunk_page_bounds: list[tuple[int, str]] = []
        # #186 section-trail invariant: a chunk's deepest section is the heading
        # under which its FIRST content sits. Snapshot the trail ONCE when the
        # chunk opens and emit that snapshot — never the live trail at the
        # closing boundary (which, on a split landing exactly on a heading, is
        # already one section ahead of the chunk's content).
        current_chunk_headings: List[str] = paragraph_to_headings.get(0, [])
        current_chunk_page_start = paragraph_to_page.get(0, "1")
        current_chunk_page_end = current_chunk_page_start
        current_chunk_phys_start = paragraph_to_phys.get(0)
        current_chunk_phys_end = current_chunk_phys_start

        for i, para in enumerate(paragraphs):
            para_tokens = self._count_tokens(para)
            para_headings = paragraph_to_headings.get(i, [])

            # Check if this is a heading paragraph
            first_line = para.strip().split('\n')[0] if para.strip() else ""
            is_heading = self._heading_pattern.match(first_line)

            # Determine if we should start a new chunk
            should_start_new = False

            if is_heading:
                # New section - emit current chunk if not empty
                if current_chunk_paras:
                    should_start_new = True
            elif current_chunk_tokens + para_tokens > self.max_chunk_tokens:
                # Would exceed budget - emit current chunk
                if current_chunk_paras:
                    should_start_new = True

            if should_start_new and current_chunk_paras:
                # Emit current chunk
                chunk_text = "\n\n".join(current_chunk_paras)
                chunk_tokens = self._count_tokens(chunk_text)

                # Check if chunk needs recursive splitting
                if chunk_tokens > self.max_chunk_tokens:
                    sub_chunks = self._recursive_split(
                        chunk_text, self.max_chunk_tokens, self.overlap_tokens
                    )
                    sub_pages = _slice_page_bounds(chunk_text, sub_chunks, current_chunk_page_bounds)
                    for sub_text, sub_bounds in zip(sub_chunks, sub_pages):
                        chunks.append(self._create_chunk(
                            sub_text, doc_id, chunk_id_counter,
                            current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                            current_chunk_headings, doc_metadata,
                            chapter=chapter_map.get(current_chunk_phys_start),
                            physical_page_start=current_chunk_phys_start,
                            physical_page_end=current_chunk_phys_end,
                            page_start=current_chunk_page_start, page_end=current_chunk_page_end,
                            paragraph_pages=sub_bounds,
                        ))
                        chunk_id_counter += 1
                else:
                    chunks.append(self._create_chunk(
                        chunk_text, doc_id, chunk_id_counter,
                        current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                        current_chunk_headings, doc_metadata,
                        chapter=chapter_map.get(current_chunk_phys_start),
                        physical_page_start=current_chunk_phys_start,
                        physical_page_end=current_chunk_phys_end,
                        page_start=current_chunk_page_start, page_end=current_chunk_page_end,
                        paragraph_pages=current_chunk_page_bounds,
                    ))
                    chunk_id_counter += 1

                # Reset page tracking for new chunk
                prev_chunk_last_page = current_chunk_page_end
                current_chunk_page_start = paragraph_to_page.get(i, current_chunk_page_end)
                current_chunk_page_end = current_chunk_page_start
                # #194: reset the page bounds; if a recycled overlap fragment
                # opens the chunk, it sits on the PREVIOUS chunk's last page
                # — seed (0, prev_page) so the map stays truthful.
                current_chunk_page_bounds = []

                # Start new chunk with overlap
                if self.overlap_tokens > 0 and current_chunk_paras:
                    # Take last portion for overlap
                    overlap_text = current_chunk_paras[-1] if current_chunk_paras else ""
                    overlap_words = overlap_text.split()
                    if len(overlap_words) > self.overlap_tokens:
                        overlap_para = " ".join(overlap_words[-self.overlap_tokens:])
                        current_chunk_paras = [overlap_para]
                        current_chunk_tokens = self._count_tokens(overlap_para)
                        current_chunk_page_bounds = [(0, str(prev_chunk_last_page))]
                    else:
                        current_chunk_paras = []
                        current_chunk_tokens = 0
                else:
                    current_chunk_paras = []
                    current_chunk_tokens = 0

                current_start_idx = i
                # #186: snapshot the trail at the moment the new chunk opens —
                # para i is its first non-overlap content; a heading para's own
                # trail already includes itself. (With overlap, the recycled
                # fragment from the previous chunk becomes its own tiny chunk
                # carrying the PREVIOUS section's trail — first content wins.)
                current_chunk_headings = para_headings
                current_chunk_phys_start = paragraph_to_phys.get(i)
                current_chunk_phys_end = current_chunk_phys_start

            # Add paragraph to current chunk
            para_offset = sum(len(p) for p in current_chunk_paras) + 2 * max(0, len(current_chunk_paras) - 1)
            current_chunk_paras.append(para)
            current_chunk_tokens += para_tokens

            # Track page range for current chunk
            para_page = paragraph_to_page.get(i, current_chunk_page_end)
            if not current_chunk_paras or len(current_chunk_paras) == 1:
                current_chunk_page_start = para_page
                if not current_chunk_page_bounds:
                    current_chunk_page_bounds = [(0, str(para_page))]
            current_chunk_page_end = para_page
            if current_chunk_page_bounds and str(para_page) != current_chunk_page_bounds[-1][1]:
                current_chunk_page_bounds.append((para_offset, str(para_page)))
            p_phys = paragraph_to_phys.get(i)
            if p_phys is not None:
                current_chunk_phys_end = p_phys

        # Emit final chunk
        if current_chunk_paras:
            chunk_text = "\n\n".join(current_chunk_paras)
            chunk_tokens = self._count_tokens(chunk_text)

            # Merge into previous if too small
            if chunk_tokens < self.min_chunk_tokens and chunks:
                # Append to last chunk
                last_chunk = chunks[-1]
                last_chunk["text"] = last_chunk["text"] + "\n\n" + chunk_text
                last_chunk["metadata"]["end_paragraph_index"] = current_start_idx + len(current_chunk_paras) - 1
                last_chunk["metadata"]["token_count"] = self._count_tokens(last_chunk["text"])
                last_chunk["metadata"]["page_end"] = current_chunk_page_end
            else:
                # Check if needs recursive splitting
                if chunk_tokens > self.max_chunk_tokens:
                    sub_chunks = self._recursive_split(
                        chunk_text, self.max_chunk_tokens, self.overlap_tokens
                    )
                    sub_pages = _slice_page_bounds(chunk_text, sub_chunks, current_chunk_page_bounds)
                    for sub_text, sub_bounds in zip(sub_chunks, sub_pages):
                        chunks.append(self._create_chunk(
                            sub_text, doc_id, chunk_id_counter,
                            current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                            current_chunk_headings, doc_metadata,
                            chapter=chapter_map.get(current_chunk_phys_start),
                            physical_page_start=current_chunk_phys_start,
                            physical_page_end=current_chunk_phys_end,
                            page_start=current_chunk_page_start, page_end=current_chunk_page_end,
                            paragraph_pages=sub_bounds,
                        ))
                        chunk_id_counter += 1
                else:
                    chunks.append(self._create_chunk(
                        chunk_text, doc_id, chunk_id_counter,
                        current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                        current_chunk_headings, doc_metadata,
                        chapter=chapter_map.get(current_chunk_phys_start),
                        physical_page_start=current_chunk_phys_start,
                        physical_page_end=current_chunk_phys_end,
                        page_start=current_chunk_page_start, page_end=current_chunk_page_end,
                        paragraph_pages=current_chunk_page_bounds,
                    ))

        return chunks

    def _chunk_paragraph_based(
        self,
        paragraphs: List[str],
        doc_metadata: Optional[Dict[str, Any]]
    ) -> List[Dict[str, Any]]:
        """Legacy paragraph-based chunking for backward compatibility."""
        chunks = []
        doc_id = doc_metadata.get("doc_id", "unknown_doc") if doc_metadata else "unknown_doc"
        chunk_id_counter = 0

        step = self.paragraphs_per_chunk - self.overlap_paragraphs
        for i in range(0, len(paragraphs), step):
            end_index = i + self.paragraphs_per_chunk
            current_paragraphs = paragraphs[i:end_index]

            if not current_paragraphs:
                continue

            chunk_text = "\n\n".join(current_paragraphs)
            chunks.append(self._create_chunk(
                chunk_text, doc_id, chunk_id_counter,
                i, min(end_index, len(paragraphs)) - 1,
                [], doc_metadata  # No section titles in legacy mode
            ))
            chunk_id_counter += 1

            if end_index >= len(paragraphs):
                break

        return chunks

    def _create_chunk(
        self,
        text: str,
        doc_id: str,
        chunk_index: int,
        start_para: int,
        end_para: int,
        section_titles: List[str],
        doc_metadata: Optional[Dict[str, Any]],
        page_start: str = "",
        page_end: str = "",
        chapter: Optional[int] = None,
        physical_page_start: Optional[int] = None,
        physical_page_end: Optional[int] = None,
        paragraph_pages: Optional[List[tuple]] = None,
    ) -> Dict[str, Any]:
        """Create a chunk dictionary with all metadata."""
        image_refs = self._extract_images_from_text(text)

        chunk_meta = {
            "doc_id": doc_id,
            "chunk_id": f"{doc_id}_chunk_{chunk_index:04d}",
            "chunk_index": chunk_index,
            "start_paragraph_index": start_para,
            "end_paragraph_index": end_para,
            "section_titles": list(section_titles),
            "token_count": self._count_tokens(text),
            "image_refs": image_refs,
            "page_start": page_start,
            "page_end": page_end,
        }
        # W12: chapter ordinal of the page the chunk's first content sits
        # on (corroborated chapter-relative books only; None otherwise).
        if chapter is not None:
            chunk_meta["chapter"] = chapter
        # W12 review C1: exact physical anchors from the Marker page markers.
        # Chapter-relative books carry DUPLICATE labels across chapters, so
        # the runner's label reverse-mapping (min of hits) would resolve to
        # the EARLIEST chapter's page — the chunker's own physical tracking
        # is the ground truth the locator must store.
        if physical_page_start is not None:
            chunk_meta["physical_page_start"] = physical_page_start
        if physical_page_end is not None:
            chunk_meta["physical_page_end"] = physical_page_end
        # #194 per-paragraph page map: [[char_offset, page_label], ...] —
        # boundaries where the chunk's print page changes (first entry always
        # (0, first page)). Consumers derive the exact page of any position;
        # the span stays as the honest envelope.
        if paragraph_pages:
            chunk_meta["paragraph_pages"] = [[str(off), str(lab)] for off, lab in paragraph_pages]

        if doc_metadata:
            # Exclude large internal fields that shouldn't be stored per-chunk
            _EXCLUDE_KEYS = {"doc_id", "page_label_map", "page_chapter_map"}
            chunk_meta.update({k: v for k, v in doc_metadata.items() if k not in _EXCLUDE_KEYS})

        return {
            "text": text,
            "metadata": chunk_meta
        }
