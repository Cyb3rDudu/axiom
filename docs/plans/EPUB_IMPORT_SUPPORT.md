# EPUB Import Support — Implementation Plan

**Status:** Draft · **Date:** 2026-07-13
**Goal:** Accept `.epub` files end-to-end (upload → convert → chunk → embed → searchable), with **images extracted and served** — implemented to mirror the existing **PDF / Marker** import path as closely as possible.

**Scope: Python backend only.** The Go (`axiom_backend_ng`) stack is out of scope for this work.

---

## 1. Design principle — "exactly analogous to PDF + Marker"

The PDF import is the template. Every EPUB mechanism has a PDF counterpart we copy:

| Concern | PDF / Marker (existing) | EPUB (new) |
|---|---|---|
| Converter engine | Marker (GPU layout/OCR) | **pandoc** (already installed: `axiom_backend/Dockerfile:32`; binding `pypandoc` in `requirements.txt`) |
| Runs in… | a short-lived per-import subprocess `ai_researcher.pdf_worker` | a short-lived per-import subprocess `ai_researcher.epub_worker` |
| Worker CLI | `<pdf> <out_md> <out_images_dir>` | `<epub> <out_md> <out_images_dir>` (identical shape) |
| Worker stdout protocol | `{ok, markdown_path, images_dir, image_mapping}` | identical |
| Image naming | `out_images_dir/image_<N>.<ext>` | identical |
| Spawned by | `BackgroundDocumentProcessor._convert_pdf_via_subprocess` | `_convert_epub_via_subprocess` (mirror) |
| Dispatch branch | `background_document_processor.py:642-667` | a new `.epub` `elif` branch beside it, reusing `_update_markdown_image_paths` |
| Image serving | `/api/images/{doc_id}/{filename}` (`documents.py:1463`) | reused unchanged |
| Storage subdir | `RAW_FILES/` (pdfs at root) | `RAW_FILES/epub_files/` (mirrors `word_documents/`, `markdown_files/`) |

The **only** material difference is the converter engine inside the worker (pandoc instead of Marker). The subprocess plumbing, JSON protocol, image extraction, image-path rewriting, and serving route are all reused verbatim.

> Why a subprocess for pandoc when pandoc needs no GPU/VRAM isolation? Purely for **architectural symmetry** with the PDF path — one worker package per format, identical CLI/protocol, uniform error handling. The overhead is negligible (pandoc converts a whole book in seconds; process spawn is milliseconds). This is the "exactly analogous" the user asked for.

---

## 2. New code — the `epub_worker` package

Mirror of `axiom_backend/ai_researcher/pdf_worker/` (`__init__.py` + `__main__.py`).

### `axiom_backend/ai_researcher/epub_worker/__init__.py`
One-line docstring mirroring `pdf_worker/__init__.py`.

### `axiom_backend/ai_researcher/epub_worker/__main__.py`
Structural copy of `pdf_worker/__main__.py` (same arg parsing, same `_stderr_err`, same final JSON line, same exit codes). The body differs only in the conversion step:

```python
# Sketch — mirrors pdf_worker/__main__.py main()
def main() -> int:
    ...
    epub_path, out_md, out_images_dir = Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3])
    out_md.parent.mkdir(parents=True, exist_ok=True)
    media_tmp = out_images_dir.parent / f".epub_media_{os.getpid()}"
    try:
        import pypandoc
        # Convert EPUB -> GFM; pandoc extracts images into media_tmp.
        pypandoc.convert_file(
            str(epub_path), to="gfm",
            outputfile=str(out_md),
            extra_params=["--wrap=none", f"--extract-media={media_tmp}"],
        )
        markdown = out_md.read_text(encoding="utf-8")
        if not markdown.strip():
            _stderr_err({"ok": False, "error": "pandoc returned empty markdown"}); return 1

        # Enumerate extracted images, rename to image_<N>.<ext> (mirror of
        # pdf_worker._save_images), build {basename_as_referenced: new_name}.
        mapping = _save_extracted_images(media_tmp, out_images_dir)

        print(json.dumps({"ok": True, "markdown_path": str(out_md),
                          "images_dir": str(out_images_dir), "image_mapping": mapping}), flush=True)
        return 0
    except Exception as exc:
        _stderr_err({"ok": False, "error": str(exc), "traceback": traceback.format_exc()}); return 1
    finally:
        shutil.rmtree(media_tmp, ignore_errors=True)
```

`_save_extracted_images(media_tmp, out_dir)`:
- Walk `media_tmp` recursively for image files (`.png/.jpg/.jpeg/.gif/.webp/.svg/.bmp`).
- Copy each to `out_dir/image_<N>.<original_ext>`, mapping `{<basename>: image_<N>.<ext>}`.
- Keys are **basenames** — exactly what `_update_markdown_image_paths` matches on (`processor.py:643` does `Path(old_path).name`).

This mirrors `pdf_worker._save_images`'s `{original_filename: saved_filename}` contract.

---

## 3. New code — `_convert_epub_via_subprocess`

Mirror of `_convert_pdf_via_subprocess` (`background_document_processor.py:264-323`), placed right after it:

```python
def _convert_epub_via_subprocess(self, doc_id, epub_path, out_md_path, out_images_dir):
    """Run the epub_worker subprocess and return (markdown, image_mapping).
    Mirror of _convert_pdf_via_subprocess. Raises RuntimeError on failure."""
    cmd = [sys.executable, "-m", "ai_researcher.epub_worker",
           str(epub_path), str(out_md_path), str(out_images_dir)]
    proc = subprocess.run(cmd, capture_output=True, text=True, env=os.environ.copy())
    if proc.returncode != 0:
        if proc.stderr: print(f"[{doc_id}] epub_worker stderr: {proc.stderr}")
        raise RuntimeError(f"epub_worker exited with code {proc.returncode}")
    # last {...} line on stdout is the result (same protocol as pdf_worker)
    last_line = next((l.strip() for l in reversed((proc.stdout or "").splitlines())
                      if l.strip().startswith("{") and l.strip().endswith("}")), "")
    if not last_line:
        raise RuntimeError(f"epub_worker produced no JSON result; stdout={proc.stdout[-400:]}")
    result = json.loads(last_line)
    if not result.get("ok"):
        raise RuntimeError(f"epub_worker reported failure: {result.get('error')}")
    return out_md_path.read_text(encoding="utf-8"), result.get("image_mapping") or {}
```

**No VRAM juggling needed** — unlike the PDF branch (`_prepare_vrm_for_gpu_subprocess` at `:649`), pandoc is CPU-only and shares no GPU memory with the embedder/reranker/GLiNER. That call is omitted on purpose.

---

## 4. Wire it into the dispatch (the core change)

In `background_document_processor.py::_process_document_sync`, add an EPUB branch that is a near-copy of the PDF branch (`:642-667`), minus VRAM prep:

```python
elif original_filename.lower().endswith('.epub'):
    print(f"[{doc_id}] Converting EPUB to Markdown via epub_worker subprocess...")
    md_filename = f"{doc_id}.md"
    md_save_path = processor.markdown_dir / md_filename
    image_dir = processor.image_dir / doc_id
    markdown_content, image_mapping = self._convert_epub_via_subprocess(
        doc_id=doc_id, epub_path=target_path,
        out_md_path=md_save_path, out_images_dir=image_dir,
    )
    if config.ENABLE_IMAGE_EXTRACTION and image_mapping:
        mapping_as_paths = {orig: image_dir / new for orig, new in image_mapping.items()}
        markdown_content = processor._update_markdown_image_paths(markdown_content, doc_id, mapping_as_paths)
        md_save_path.write_text(markdown_content, encoding="utf-8")
        extracted_images = mapping_as_paths
        print(f"[{doc_id}] Organized {len(image_mapping)} images")
```

And the **storage-routing** `if/elif` at `:579-590` gets an EPUB arm:
```python
elif original_filename.lower().endswith('.epub'):
    epub_dir = self.pdf_dir / 'epub_files'
    epub_dir.mkdir(parents=True, exist_ok=True)
    target_path = epub_dir / f"{doc_id}_{original_filename}"
```

Everything after the conversion branch — chunking, embedding, entity extraction, status marking — is format-agnostic and **needs no change**; it operates on the produced `{doc_id}.md`, exactly as for PDF/DOCX/MD.

---

## 5. Metadata preview step (mirror PDF's pre-conversion extract)

PDF extracts a cheap text preview *before* the expensive Marker run (`:602-603`, via `_extract_header_footer_text`). The EPUB analog: a cheap pandoc→plain conversion for the metadata/title/author extractor, without invoking the full worker.

In `document_converter.py`:
- `is_epub_file(filename)` → `filename.lower().endswith('.epub')`.
- Update `is_supported_format()` (`:37-41`) to include `.epub`.
- Add an EPUB branch in `extract_initial_text_for_metadata()` (`:156-219`):
  ```python
  elif self.is_epub_file(filename):
      import pypandoc
      text = pypandoc.convert_file(str(file_path), to="plain")  # cheap, no images
      return text[:2000]
  ```

This feeds the existing metadata-enrichment + retry flow (`:627-704`) unchanged.

---

## 6. The remaining gatekeepers (accept-and-store prerequisites)

These just append `.epub` to an existing list / add one `elif` arm. All in Python:

| File | Site | Change |
|---|---|---|
| `axiom_backend/api/documents.py` | `:240` and `:361` | add `.epub` to `supported_extensions` (both upload endpoints) |
| `axiom_backend/api/documents.py` | `:245`, `:366` | extend the "Only PDF, Word …" error string to mention EPUB |
| `axiom_backend/database/crud_documents_improved.py` | `:59-68` | add `elif …('.epub'): file_dir = … / 'epub_files'` before the `else: raise` |
| `axiom_backend/ai_researcher/core_rag/document_converter.py` | `:37-41`, `:156-219` | per §5 |
| `axiom_backend/ai_researcher/core_rag/processor.py` | `:1330-1338` dispatch + `:1355-1368` glob (`*.epub`) | add EPUB branch + glob entry (parity for the RAG-processor/CLI path; the live upload path is the background processor) |

### Secondary / optional (not required for the live upload flow)
- `axiom_backend/cli_ingest*.py` — glob list + "Supported formats" message for bulk CLI ingest. **Verify which of `cli_ingest.py` / `_fixed.py` / `_backup.py` is live first** (`_backup` is almost certainly dead).
- `axiom_backend/services/metadata_enrichment.py:713,721` + `axiom_frontend/.../metadataCompleteness.ts` — academic/web classification; an EPUB would currently classify as `'web'`. Decide later whether to add a `'book'` bucket (helps OpenLibrary ISBN enrichment). Out of scope for parity.
- Frontend (`DocumentUploadZone.tsx`, `DocumentsPage.tsx`, `EnhancedDocumentList.tsx`) — add `.epub` + `'application/epub+zip'` to the accept/validate lists so the UI lets the user pick EPUBs. Strictly required for the feature to be usable, but it's UI plumbing, not the Python pipeline.

---

## 7. EPUB-specific edge cases

- **DRM / encryption** (library loans, Adobe DRM): pandoc fails → the subprocess returns non-zero → `_convert_epub_via_subprocess` raises → the pool marks the row `failed` with the error string (existing mechanism). Surface a clear message ("EPUB appears DRM-protected or corrupt").
- **Images**: handled with full PDF parity via `--extract-media` → `image_<N>.<ext>` → `/api/images/<doc_id>/…`. EPUB covers and inline figures render in the document viewer exactly like PDF figures.
- **Size**: full novels can be large; pandoc handles them in seconds; the chunker + existing markdown-byte clamp bound memory. No new timeout needed (pandoc is fast; the Marker 9h ceiling doesn't apply).
- **Format scope**: standard `.epub` only. `.mobi` / `.azw3` / `.kf8` (Kindle) are **out of scope** — different formats, rejected by the allowlist.
- **OPF metadata (future)**: EPUB's `content.opf` carries title/author/ISBN. Feeding the ISBN into the existing OpenLibrary enrichment would give near-perfect book metadata — a strong follow-up, not required for parity.

---

## 8. Testing

- **Unit — `epub_worker`**: commit a tiny sample EPUB (a 2-3 chapter public-domain book, or a generated fixture) under `tests/`. Assert `python -m ai_researcher.epub_worker sample.epub out.md img/` → exit 0, `out.md` non-empty, JSON has `ok:true`, and images land as `image_N.<ext>`. Assert a corrupt/zip-bomb file → non-zero exit with JSON error.
- **Unit — `_convert_epub_via_subprocess`**: with the worker stubbed, assert it parses the JSON protocol and raises `RuntimeError` on `ok:false` / missing line.
- **Integration — `background_document_processor`**: feed a sample EPUB through `_process_document_sync` and assert it reaches `processing_status='completed'` with `{doc_id}.md` produced and (if the sample has images) `/api/images/{doc_id}/` populated.
- **API**: assert `POST /api/documents/upload` with `.epub` returns 200 (not 400), and a non-`.epub`-of-the-supported-set still 400s.

---

## 9. Files touched (Python only)

**New**
- `axiom_backend/ai_researcher/epub_worker/__init__.py`
- `axiom_backend/ai_researcher/epub_worker/__main__.py`
- `tests/` sample EPUB fixture + tests

**Modified**
- `axiom_backend/services/background_document_processor.py` (new `_convert_epub_via_subprocess`; storage-routing arm; conversion-dispatch arm)
- `axiom_backend/ai_researcher/core_rag/document_converter.py` (`is_epub_file`, `is_supported_format`, metadata-preview branch)
- `axiom_backend/ai_researcher/core_rag/processor.py` (dispatch + glob)
- `axiom_backend/database/crud_documents_improved.py` (storage routing)
- `axiom_backend/api/documents.py` (allowlist ×2 + error strings)
- *Frontend (UI plumbing, not pipeline):* `DocumentUploadZone.tsx`, `DocumentsPage.tsx`, `EnhancedDocumentList.tsx`
- *Docs:* `docs/index.md`, `docs/user-guide/documents/uploading.md`, `docs/DEV_MACOS.md` (add `epub_files` to the `mkdir` layout)

**No new Python or system dependencies** — `pandoc` (binary) and `pypandoc` (binding) are already installed.
