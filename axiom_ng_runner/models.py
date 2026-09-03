"""Wire models for the processor contract v1.

Strict pydantic models for the *small* request/response envelopes where the
contract fixes the shape (accept, status, ack, error, capability header).
The processor *result* payload is intentionally NOT a strict model here: the
contract (§4) requires additive unknown fields to be ignored by readers, and
the strict validation of every result field is axiom-ng's job at Gate 4, not
the processor's.
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel, ConfigDict, Field


def _no_extra() -> ConfigDict:
    return ConfigDict(extra="forbid")


# --- Process request (POST /v1/process body, contract §7) -----------------


class SourceInfo(BaseModel):
    model_config = _no_extra()
    type: str = "zotero"
    source_id: str
    server_id: str


class MetadataSnapshot(BaseModel):
    model_config = ConfigDict(extra="allow")
    # Contract-conformant subset; unknown biblio keys are preserved verbatim.


class DocumentInfo(BaseModel):
    model_config = ConfigDict(extra="allow")
    document_id: str
    zotero_key: str
    zotero_version: int
    metadata_snapshot: MetadataSnapshot = Field(default_factory=MetadataSnapshot)


class AttachmentInfo(BaseModel):
    model_config = _no_extra()
    attachment_id: str
    zotero_key: str
    zotero_version: int
    content_type: str
    filename: str
    local_path: str
    # Optional HMAC-signed download URL (remote delivery): used only when
    # local_path is not locally accessible. Declared explicitly because the
    # model forbids unknown fields.
    source_url: str | None = None
    content_hash: str
    size_bytes: int = 0
    mtime_ms: int = 0


class ProcessingOptions(BaseModel):
    model_config = _no_extra()
    profile: str = "full-rag-v1"
    force_rebuild: bool = False
    language_hint: str | None = None
    extract_images: bool = False
    compute_dense_embeddings: bool = False
    compute_sparse_embeddings: bool = False
    extract_entities: bool = False
    extract_relationships: bool = False
    # #230: machine captions for extracted_image artifacts (default off —
    # the profile is the admin gate; without the flag behavior is
    # byte-identical).
    extract_image_captions: bool = False


class ProcessRequest(BaseModel):
    """Contract §7. ``metadata_snapshot`` allows additive fields."""

    model_config = ConfigDict(extra="forbid")
    contract_version: str
    job_id: str
    idempotency_key: str
    source: SourceInfo
    document: DocumentInfo
    attachment: AttachmentInfo
    processing: ProcessingOptions = Field(default_factory=ProcessingOptions)


# --- Acceptance (POST /v1/process response, contract §8) ------------------


class ProcessAccept(BaseModel):
    model_config = _no_extra()
    contract_version: str
    job_id: str
    status: str
    deduplicated: bool = False


# --- Job status (GET /v1/jobs/{id}, contract §9) --------------------------


class Progress(BaseModel):
    model_config = _no_extra()
    completed_units: int = 0
    total_units: int = 0
    unit: str = ""


class ProcessingError(BaseModel):
    model_config = _no_extra()
    code: str
    message: str
    retryable: bool = False
    stage: str | None = None
    details: dict[str, Any] = Field(default_factory=dict)


class JobStatus(BaseModel):
    """Contract §9. ``status`` is validated against the allowed set."""

    model_config = _no_extra()
    contract_version: str
    job_id: str
    status: str
    stage: str | None = None
    progress: Progress = Field(default_factory=Progress)
    # #225 early-commit: true once chunks/embeddings are committed and a
    # late stage (relationships) is still running or has failed — the
    # partial result exists, the work is not lost.
    partial_result_available: bool = False
    error: ProcessingError | None = None
    started_at: str | None = None
    updated_at: str | None = None
    completed_at: str | None = None


# --- Query embedding (POST /v1/embed, contract §7a additive, #131) -------


class EmbedRequest(BaseModel):
    """Contract §7a. ``max_texts`` is an optional per-request cap; the
    server-side limit (capabilities.limits.max_query_texts) always wins.
    ``include_sparse`` (R5 #135) additionally returns the learned lexical
    weights per text — the query side of the OS rank_features arm."""

    model_config = _no_extra()
    contract_version: str
    texts: list[str]
    max_texts: int | None = None
    include_sparse: bool = False


class EmbedResponse(BaseModel):
    model_config = _no_extra()
    contract_version: str
    model: str
    dimensions: int
    embeddings: list[list[float]]
    # Present only when the request asked include_sparse (R5 #135): one
    # {token: weight} map per input text, aligned with ``embeddings``.
    sparse: list[dict[str, float]] | None = None


# --- Rerank (POST /v1/rerank, contract §7a additive, #132) -----------------


class RerankRequest(BaseModel):
    """Contract §7a. ``top_n`` must be >= 1; values above ``len(texts)`` are
    clamped (archive slicing semantics — return all texts)."""

    model_config = _no_extra()
    contract_version: str
    query: str
    texts: list[str]
    top_n: int = 10


class RerankScore(BaseModel):
    model_config = _no_extra()
    index: int
    score: float


class RerankResponse(BaseModel):
    model_config = _no_extra()
    contract_version: str
    model: str
    scores: list[RerankScore]


# --- Preflight (POST /v1/pdf/preflight, #175) ------------------------------


class PreflightReport(BaseModel):
    """Built from pdf_health.analyze_pdf: a READ-ONLY quality diagnostic of a
    PDF BEFORE chunking (#175). No repair, no mutation of upstream state.
    ok=False means the quality gate rejects it (repair-case / skip policy);
    the nested `details` mirrors analyze_pdf (labels, folio runs, versatz,
    text-layer metrics)."""
    model_config = ConfigDict(extra="allow")
    contract_version: str
    source_name: str
    ok: bool
    finding: str
    reason: str
    details: dict[str, Any] = Field(default_factory=dict)


# --- Ack (POST /v1/jobs/{id}/ack, contract §15) ----------------------------


class AckPayload(BaseModel):
    model_config = _no_extra()
    persisted: bool
    snapshot_id: str | None = None


class AckResponse(BaseModel):
    model_config = _no_extra()
    contract_version: str
    job_id: str
    status: str  # "acked"
    ok: bool = True


# --- Capabilities (GET /v1/capabilities, contract §6) ----------------------


class Capabilities(BaseModel):
    model_config = _no_extra()
    contract_versions: list[str]
    processor: dict[str, str]
    formats: list[str]
    features: dict[str, bool]
    models: dict[str, dict[str, Any]]  # heterogeneous: name:str, dimensions:int
    limits: dict[str, Any]
    # #216 honest readiness: models_warmed is the ACTUAL preload state of the
    # query models (False while warming or if not real-loaded); warmup_enabled
    # reflects AXIOM_PROCESSOR_WARMUP. Additive contract fields — older
    # consumers ignore them.
    warmup_enabled: bool = False
    models_warmed: bool = False
