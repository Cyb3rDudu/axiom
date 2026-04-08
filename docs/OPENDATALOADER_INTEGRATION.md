# OpenDataLoader PDF Integration Plan

## Overview

[OpenDataLoader PDF](https://github.com/opendataloader-project/opendataloader-pdf) is a high-accuracy PDF parser that converts PDFs into structured formats (Markdown, JSON, HTML) with correct reading order, table extraction, and bounding boxes. It ranks #1 in benchmarks (0.907 accuracy across 200 real-world PDFs) and runs on **CPU only** (no GPU required).

This document plans the integration of OpenDataLoader as a **second PDF parser** alongside Marker in Axiom's document processing pipeline.

## Why Add a Second Parser?

| Aspect | Marker (current) | OpenDataLoader |
|--------|------------------|----------------|
| **GPU** | Required (~2.5GB VRAM) | CPU only (Java-based) |
| **Speed** | ~1-3s/page (GPU) | ~0.05s/page local, ~0.46s/page hybrid |
| **Tables** | Good with table recognition mode | Best-in-class (0.928 accuracy) |
| **Reading order** | Good | Best-in-class (0.934 accuracy) |
| **Page markers** | `paginate_output: True` → `{N}---` markers | JSON output includes `page number` per element |
| **Bounding boxes** | Not available | Per-element bounding boxes in JSON output |
| **OCR** | Built-in (surya) | Via hybrid mode (80+ languages) |
| **Dependencies** | Python + PyTorch + GPU | Java 11+ (JVM), pip package wraps it |
| **License** | GPL-3.0 | Apache 2.0 |

### Key Benefits

1. **No GPU needed for PDF conversion** — frees ~2.5GB VRAM during imports. Critical for 12GB GPU deployments where Marker + embedder + mREBEL compete for VRAM.
2. **Better table extraction** — academic papers and legal documents often have complex tables.
3. **Bounding boxes** — enables future features like visual citation highlighting.
4. **Page numbers per element** — cleaner page label mapping than Marker's `{N}---` regex parsing.
5. **Prompt injection filtering** — sanitizes hidden text layers that could poison RAG context.

## Integration Architecture

### Parser Selection

Add a `PDF_PARSER` config option:

```python
# config.py
PDF_PARSER = os.getenv("PDF_PARSER", "marker")  # "marker", "opendataloader", or "auto"
```

- `"marker"` — Current behavior (GPU-based, proven)
- `"opendataloader"` — CPU-based, better tables, no GPU
- `"auto"` — Use OpenDataLoader by default, fall back to Marker on failure

### Integration Point: `processor.py`

The current flow in `process_pdf()` (line 809-823):

```python
if markdown_content is None:
    markdown_content, marker_images = self._convert_pdf_with_table_handling(pdf_path)
```

Replace with parser dispatch:

```python
if markdown_content is None:
    if self.pdf_parser == "opendataloader":
        markdown_content, page_map, images = self._convert_pdf_opendataloader(pdf_path)
    else:
        markdown_content, images = self._convert_pdf_with_table_handling(pdf_path)
```

### New Method: `_convert_pdf_opendataloader()`

```python
def _convert_pdf_opendataloader(self, pdf_path: Path) -> Tuple[str, Dict[int, str], Dict]:
    """Convert PDF using OpenDataLoader. Returns (markdown, page_label_map, images)."""
    import opendataloader_pdf

    with tempfile.TemporaryDirectory() as tmpdir:
        opendataloader_pdf.convert(
            input_path=[str(pdf_path)],
            output_dir=tmpdir,
            format="markdown,json",
            image_output="external",
        )

        # Read markdown output
        md_files = list(Path(tmpdir).glob("*.md"))
        markdown_content = md_files[0].read_text() if md_files else ""

        # Read JSON for page numbers and bounding boxes
        json_files = list(Path(tmpdir).glob("*.json"))
        page_map = {}
        if json_files:
            elements = json.loads(json_files[0].read_text())
            # Build page label map from JSON elements
            for elem in elements:
                page_num = elem.get("page number")
                if page_num is not None:
                    page_map[page_num - 1] = str(page_num)  # 0-indexed → label

        # Collect extracted images
        images = {}
        img_dir = Path(tmpdir) / "images"
        if img_dir.exists():
            for img_path in img_dir.iterdir():
                images[img_path.name] = img_path.read_bytes()

    return markdown_content, page_map, images
```

### Page Number Integration

OpenDataLoader's JSON output includes `page number` per element, which is more reliable than Marker's `{N}---` page marker regex. The page map can feed directly into the chunker's `page_label_map` without the 3-tier fallback logic.

### Marker Page Markers vs OpenDataLoader

| Feature | Marker | OpenDataLoader |
|---------|--------|----------------|
| Page info source | `{N}---` markers in markdown | `page number` field in JSON |
| Accuracy | Depends on regex parsing | Exact (from PDF structure) |
| Integration | `_PAGE_MARKER_RE` in chunker.py | Direct page_label_map from JSON |

## Implementation Steps

### Phase 1: Basic Integration (MVP)

1. **Add dependency**: `pip install opendataloader-pdf` to requirements.txt
2. **Docker**: Ensure Java 11+ is in the container (add `default-jre-headless` to Dockerfile apt-get)
3. **Config**: Add `PDF_PARSER` env var to config.py
4. **Processor**: Add `_convert_pdf_opendataloader()` method
5. **Parser dispatch**: Route based on `PDF_PARSER` config
6. **Page labels**: Use JSON output for page mapping when OpenDataLoader is active

### Phase 2: Hybrid Mode (Optional)

1. **Sidecar container**: Run `opendataloader-pdf-hybrid --port 5002` as a separate service
2. **Complex page routing**: Automatically route pages with tables/formulas to hybrid mode
3. **Docker compose**: Add opendataloader-hybrid service

### Phase 3: Bounding Box Features (Future)

1. **Store bounding boxes**: Add `bounding_box` field to chunk metadata
2. **Visual citations**: Frontend can highlight exact regions in PDF viewer
3. **Element-level retrieval**: Search by heading, table, or paragraph

## Docker Changes

### Dockerfile Addition

```dockerfile
# Add Java runtime for OpenDataLoader
RUN apt-get update && apt-get install -y --no-install-recommends \
    default-jre-headless \
    && rm -rf /var/lib/apt/lists/*
```

### requirements.txt Addition

```
opendataloader-pdf>=1.0.0
```

### docker-compose.gpu-external-db.yml

```yaml
environment:
  - PDF_PARSER=${PDF_PARSER:-marker}  # or "opendataloader" or "auto"
```

## VRAM Impact

| Scenario | Marker | OpenDataLoader |
|----------|--------|----------------|
| PDF conversion | ~2.5GB GPU | 0GB GPU (CPU only) |
| During import (with embedder) | ~4.7GB | ~2.2GB (embedder only) |
| Peak concurrent (import + query) | ~7-12GB | ~4.4-5GB |

On a **12GB GPU**: OpenDataLoader eliminates the Marker VRAM pressure entirely, leaving room for embedder + reranker + GLiNER without contention.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Java dependency adds ~200MB to Docker image | Build size | Use `default-jre-headless` (minimal) |
| JVM startup overhead (~2-3s first call) | Latency | Batch files in single `convert()` call |
| Different markdown format than Marker | Chunker compatibility | Test chunker with both outputs, adjust if needed |
| No `paginate_output` equivalent | Page markers | Use JSON output for page mapping instead |
| Hybrid mode needs external API key | Cost | Local mode is free, hybrid is optional |
| OpenDataLoader is newer, less battle-tested | Reliability | Keep Marker as fallback with `"auto"` mode |

## Migration Path

1. **Deploy with `PDF_PARSER=marker`** (default, no change)
2. **Test with `PDF_PARSER=opendataloader`** on a few documents
3. **Compare output quality** (tables, reading order, page numbers)
4. **Switch to `PDF_PARSER=auto`** once validated (OpenDataLoader primary, Marker fallback)
5. **Optionally remove Marker** once OpenDataLoader is proven stable

## Files to Modify

| File | Change |
|------|--------|
| `requirements.txt` | Add `opendataloader-pdf` |
| `Dockerfile` | Add `default-jre-headless` |
| `config.py` | Add `PDF_PARSER` setting |
| `core_rag/processor.py` | Add `_convert_pdf_opendataloader()`, parser dispatch |
| `core_rag/chunker.py` | Handle OpenDataLoader markdown format (if different) |
| `docker-compose.gpu-external-db.yml` | Add `PDF_PARSER` env var |
