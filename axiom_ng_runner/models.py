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
    error: ProcessingError | None = None
    started_at: str | None = None
    updated_at: str | None = None
    completed_at: str | None = None


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
