"""
Standalone RAG chat function.

Extracts the core document-grounded Q&A logic from
``UserInteractionManager._handle_chat_intent()`` so it can be reused
by both the existing WebSocket chat endpoint and the new
OpenAI-compatible REST endpoint.
"""

import logging
import re
from typing import Any, Dict, List, Optional, Tuple

logger = logging.getLogger(__name__)


async def rag_chat_response(
    user_message: str,
    conversation_history: List[Tuple[str, str]],
    document_group_id: Optional[str],
    model_dispatcher: Any,
    doc_search_tool: Any,
    temperature: float = 0.7,
    max_tokens: int = 2000,
) -> Dict[str, Any]:
    """Run a single RAG-grounded chat turn and return the result.

    Parameters
    ----------
    user_message : str
        The latest user query.
    conversation_history : list of (user_msg, assistant_msg) tuples
        Previous turns for conversational context (most recent last).
    document_group_id : str or None
        Optional document group to restrict retrieval scope.
    model_dispatcher : ModelDispatcher
        Configured dispatcher for calling the LLM.
    doc_search_tool : DocumentSearchTool
        Initialised search tool backed by a Retriever.
    temperature : float
        LLM sampling temperature.
    max_tokens : int
        Max tokens for the LLM response.

    Returns
    -------
    dict with keys:
        content : str   – The assistant response text.
        sources : list   – Structured source metadata dicts.
        usage   : dict   – Token usage (prompt_tokens, completion_tokens, total_tokens).
    """

    # ── Step 1: Fetch document metadata ──────────────────────────────
    doc_metadata_summary = ""
    doc_title_lookup: Dict[str, str] = {}
    doc_meta_lookup: Dict[str, Dict] = {}
    doc_id_to_meta: Dict[str, Dict] = {}

    try:
        from database.database import get_db
        from sqlalchemy import text as sql_text

        db = next(get_db())
        try:
            filter_clause = ""
            params: Dict[str, Any] = {}
            if document_group_id:
                filter_clause = (
                    "JOIN document_group_association dga "
                    "ON d.id = dga.document_id "
                    "WHERE dga.document_group_id = :gid"
                )
                params["gid"] = document_group_id

            rows = db.execute(
                sql_text(
                    f"SELECT d.original_filename, d.metadata_, d.id::text "
                    f"FROM documents d {filter_clause} "
                    f"ORDER BY d.created_at DESC LIMIT 50"
                ),
                params,
            ).fetchall()

            if rows:
                doc_lines = []
                for row in rows:
                    meta = row[1] or {}
                    title = meta.get("title") or row[0]
                    authors = meta.get("authors", [])
                    year = meta.get("publication_year", "")
                    doc_type = meta.get("document_type", "")
                    journal = meta.get("journal_or_source", "")
                    doi = meta.get("doi", "")
                    author_str = (
                        ", ".join(authors) if isinstance(authors, list) else str(authors)
                    )
                    meta_entry = {"title": title, "authors": authors, "year": year}
                    doc_title_lookup[row[0]] = title
                    doc_meta_lookup[row[0]] = meta_entry
                    if len(row) > 2 and row[2]:
                        doc_id_to_meta[row[2]] = meta_entry
                    line = f"- {title}\n  Authors: {author_str} | Year: {year} | Type: {doc_type}"
                    if journal:
                        line += f" | Journal: {journal}"
                    if doi:
                        line += f" | DOI: {doi}"
                    doc_lines.append(line)
                doc_metadata_summary = "\n".join(doc_lines)
        finally:
            db.close()
    except Exception as e:
        logger.debug(f"Failed to fetch document metadata: {e}")

    # ── Step 2: Search chunks ────────────────────────────────────────
    document_context = ""
    source_references: List[str] = []
    structured_sources: List[Dict[str, Any]] = []

    try:
        search_results = await doc_search_tool.execute(
            query=user_message,
            document_group_id=document_group_id,
            n_results=8,
            use_reranker=True,
        )
    except Exception as e:
        logger.error(f"RAG chat: chunk search failed: {e}", exc_info=True)
        search_results = []

    if search_results:
        MAX_RAG_CONTEXT_CHARS = 12000
        context_parts = []
        total_chars = 0

        for i, chunk in enumerate(search_results, 1):
            chunk_text = chunk.get("text", "")
            metadata = chunk.get("metadata", {})
            source = metadata.get("original_filename") or "Unknown"
            doc_id = metadata.get("doc_id") or chunk.get("doc_id") or ""
            auth_meta = doc_meta_lookup.get(source) or doc_id_to_meta.get(doc_id, {})
            title = auth_meta.get("title") or source
            authors = auth_meta.get("authors") or metadata.get("authors", [])
            year = auth_meta.get("year") or metadata.get("publication_year", "")
            section_titles = metadata.get("section_titles", [])
            section = section_titles[-1] if section_titles else ""

            max_per_chunk = MAX_RAG_CONTEXT_CHARS // 8
            if len(chunk_text) > max_per_chunk:
                chunk_text = chunk_text[:max_per_chunk] + "\n[... truncated]"

            chunk_context = f"[Source {i} -- from: {title}]\n{chunk_text}"

            image_refs = metadata.get("image_refs", [])
            if image_refs:
                img_lines = [
                    f"[Available image: {r.get('alt_text', 'Figure')}](url:{r.get('path', '')})"
                    for r in image_refs
                    if r.get("path")
                ]
                if img_lines:
                    chunk_context += "\n\nImages:\n" + "\n".join(img_lines)

            total_chars += len(chunk_context)
            if total_chars > MAX_RAG_CONTEXT_CHARS:
                break
            context_parts.append(chunk_context)

            author_str = ", ".join(authors) if isinstance(authors, list) else str(authors)

            page_num = ""
            for st in section_titles:
                page_match = re.search(r"page-(\d+)", str(st))
                if page_match:
                    page_num = page_match.group(1)
                    break

            clean_section = section
            if clean_section:
                clean_section = re.sub(r"\*+", "", clean_section)
                clean_section = re.sub(r"<[^>]+>", "", clean_section)
                clean_section = clean_section.strip()
                if len(clean_section) < 3 or clean_section.startswith("http"):
                    clean_section = ""

            ref = f"- **[{i}]** {title}"
            if author_str:
                ref += f" -- {author_str}"
            if year:
                ref += f" ({year})"
            if clean_section:
                ref += f', Kap. "{clean_section}"'
            if page_num:
                ref += f", S. {page_num}"
            source_references.append(ref)

            structured_sources.append(
                {
                    "title": title,
                    "authors": author_str,
                    "year": str(year),
                    "page": page_num,
                    "section": clean_section,
                    "doc_id": doc_id,
                }
            )

        document_context = "\n\n---\n\n".join(context_parts)

    # If nothing found at all, return a fallback message
    if not doc_metadata_summary and not document_context:
        return {
            "content": "I could not find any documents to answer your question. Please upload documents first or check your document group selection.",
            "sources": [],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
        }

    # ── Step 3: Build unified prompt ─────────────────────────────────
    doc_library_section = ""
    if doc_metadata_summary:
        doc_library_section = f"""
DOCUMENT LIBRARY (authoritative -- this is the complete list of documents you have access to):
{doc_metadata_summary}
"""

    excerpts_section = ""
    if document_context:
        ref_list = "\n".join(source_references)
        excerpts_section = f"""
RELEVANT TEXT EXCERPTS (passages FROM the documents listed above -- use these to answer content questions):
{document_context}

SOURCE REFERENCE LIST (use these for citations at the end of your response):
{ref_list}
"""

    rag_system_prompt = f"""You are a helpful assistant that answers questions based on the user's document library.

{doc_library_section}{excerpts_section}RULES:
- For questions about what documents exist, authors, titles, years, or metadata: answer from the DOCUMENT LIBRARY section above.
- For questions about document content, arguments, or topics: answer from the TEXT EXCERPTS section above and cite sources using [1], [2], etc.
- At the end of your response, include a "Quellen" section as a markdown bullet list. For each [N] you cited, add a bullet point using the SOURCE REFERENCE LIST entry. Format: `- **[N]** Title -- Authors (Year), Kapitel: "..."`. Do NOT modify source titles or authors.
- References and citations mentioned WITHIN the text excerpts are NOT documents in your library. Only the DOCUMENT LIBRARY list is authoritative.
- Do NOT invent or hallucinate document titles. If you don't know, say so.
- Images: The context may contain image references marked as [Available image: description](url:path).
  - When the user asks generally about graphics/images/figures: list what's available by description, ask which ones they want to see. Do NOT show all images at once.
  - When the user asks for a specific figure or diagram: only show it if the image path is explicitly referenced in the text excerpts. Do NOT guess which image file corresponds to a figure number.
  - If you cannot determine which image matches a requested figure, say so honestly.
  - Never show journal headers, logos, or decorative images -- only figures, charts, diagrams, and tables."""

    rag_messages: List[Dict[str, str]] = [
        {"role": "system", "content": rag_system_prompt},
    ]

    # Inject conversation history
    if conversation_history:
        for hist_user, hist_assistant in conversation_history[-3:]:
            rag_messages.append({"role": "user", "content": hist_user})
            rag_messages.append({"role": "assistant", "content": hist_assistant})

    rag_messages.append({"role": "user", "content": user_message})

    # ── Step 4: Call LLM ─────────────────────────────────────────────
    usage = {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}

    try:
        rag_response, rag_model_details = await model_dispatcher.dispatch(
            messages=rag_messages,
            agent_mode="messenger",
        )
    except Exception as e:
        logger.error(f"RAG chat LLM call failed: {e}", exc_info=True)
        return {
            "content": f"An error occurred while generating a response: {e}",
            "sources": structured_sources,
            "usage": usage,
        }

    if rag_response and rag_response.choices and rag_response.choices[0].message.content:
        response_text = rag_response.choices[0].message.content
        # Convert internal image URL placeholders
        response_text = re.sub(
            r"!\[([^\]]*)\]\(url:(/api/images/[^)]+)\)", r"![\1](\2)", response_text
        )

        if rag_model_details:
            usage["prompt_tokens"] = getattr(rag_model_details, "prompt_tokens", 0) or 0
            usage["completion_tokens"] = getattr(rag_model_details, "completion_tokens", 0) or 0
            usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]

        return {
            "content": response_text,
            "sources": structured_sources,
            "usage": usage,
        }

    return {
        "content": "The language model returned an empty response. Please try again.",
        "sources": structured_sources,
        "usage": usage,
    }
