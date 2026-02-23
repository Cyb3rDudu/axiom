import re
import os
from typing import List, Dict, Any, Optional

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

        print(f"Chunker initialized: {logger_msg}")

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

        # Choose chunking mode
        if self._use_token_chunking:
            return self._chunk_token_based(paragraphs, paragraph_to_headings, doc_metadata)
        else:
            return self._chunk_paragraph_based(paragraphs, doc_metadata)

    def _chunk_token_based(
        self,
        paragraphs: List[str],
        paragraph_to_headings: Dict[int, List[str]],
        doc_metadata: Optional[Dict[str, Any]]
    ) -> List[Dict[str, Any]]:
        """Token-based structure-aware chunking."""
        chunks = []
        doc_id = doc_metadata.get("doc_id", "unknown_doc") if doc_metadata else "unknown_doc"
        chunk_id_counter = 0

        current_chunk_paras = []
        current_chunk_tokens = 0
        current_start_idx = 0
        heading_stack = []

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
                # Update heading stack for new section
                heading_stack = para_headings
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
                    for sub_text in sub_chunks:
                        chunks.append(self._create_chunk(
                            sub_text, doc_id, chunk_id_counter,
                            current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                            heading_stack, doc_metadata
                        ))
                        chunk_id_counter += 1
                else:
                    chunks.append(self._create_chunk(
                        chunk_text, doc_id, chunk_id_counter,
                        current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                        heading_stack, doc_metadata
                    ))
                    chunk_id_counter += 1

                # Start new chunk with overlap
                if self.overlap_tokens > 0 and current_chunk_paras:
                    # Take last portion for overlap
                    overlap_text = current_chunk_paras[-1] if current_chunk_paras else ""
                    overlap_words = overlap_text.split()
                    if len(overlap_words) > self.overlap_tokens:
                        overlap_para = " ".join(overlap_words[-self.overlap_tokens:])
                        current_chunk_paras = [overlap_para]
                        current_chunk_tokens = self._count_tokens(overlap_para)
                    else:
                        current_chunk_paras = []
                        current_chunk_tokens = 0
                else:
                    current_chunk_paras = []
                    current_chunk_tokens = 0

                current_start_idx = i

            # Add paragraph to current chunk
            current_chunk_paras.append(para)
            current_chunk_tokens += para_tokens

            # Update heading stack if not a heading itself
            if not is_heading:
                heading_stack = para_headings

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
            else:
                # Check if needs recursive splitting
                if chunk_tokens > self.max_chunk_tokens:
                    sub_chunks = self._recursive_split(
                        chunk_text, self.max_chunk_tokens, self.overlap_tokens
                    )
                    for sub_text in sub_chunks:
                        chunks.append(self._create_chunk(
                            sub_text, doc_id, chunk_id_counter,
                            current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                            heading_stack, doc_metadata
                        ))
                        chunk_id_counter += 1
                else:
                    chunks.append(self._create_chunk(
                        chunk_text, doc_id, chunk_id_counter,
                        current_start_idx, current_start_idx + len(current_chunk_paras) - 1,
                        heading_stack, doc_metadata
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
        doc_metadata: Optional[Dict[str, Any]]
    ) -> Dict[str, Any]:
        """Create a chunk dictionary with all metadata."""
        image_refs = self._extract_images_from_text(text)

        chunk_meta = {
            "doc_id": doc_id,
            "chunk_id": f"{doc_id}_chunk_{chunk_index:04d}",
            "chunk_index": chunk_index,
            "start_paragraph_index": start_para,
            "end_paragraph_index": end_para,
            "section_titles": section_titles,
            "token_count": self._count_tokens(text),
            "image_refs": image_refs
        }

        if doc_metadata:
            chunk_meta.update({k: v for k, v in doc_metadata.items() if k != "doc_id"})

        return {
            "text": text,
            "metadata": chunk_meta
        }
