"""Source validation for the processor boundary (contract §7, §18).

Security-sensitive: the processor is handed a host path by Go and must not
follow it blindly. These checks run before any compute and independently of
Go's own preflight.
"""

from __future__ import annotations

import hashlib
import os
from pathlib import Path


class SourceError(Exception):
    """Raised when a source path is rejected. ``code`` maps to contract §16."""

    code: str
    message: str

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


_CONTRACT_FORMATS = {
    "application/pdf",
    "application/epub+zip",
}


def ensure_allowed_path(local_path: str, allowed_roots: tuple[str, ...]) -> Path:
    """Resolve an absolute path and ensure it sits under an allowed root.

    Rejects path traversal, symlink escapes and anything outside the allowed
    roots. Returns the resolved regular path.
    """
    raw = Path(local_path)
    if not raw.is_absolute():
        raise SourceError("SOURCE_NOT_FOUND", f"path must be absolute: {local_path!r}")

    # Resolve symlinks so a link pointing outside the roots is caught.
    try:
        resolved = raw.resolve(strict=True)
    except FileNotFoundError as err:
        raise SourceError(
            "SOURCE_NOT_FOUND", f"source not found: {local_path}"
        ) from err
    except RuntimeError as err:
        raise SourceError(
            "SOURCE_NOT_FOUND", f"cannot resolve source: {local_path}"
        ) from err

    if not allowed_roots:
        # No roots configured: refuse rather than silently allowing anything.
        raise SourceError(
            "SOURCE_NOT_FOUND",
            "no allowed source roots configured; refusing to read arbitrary path",
        )

    allowed = [Path(r).resolve(strict=False) for r in allowed_roots]
    if not any(_is_under(resolved, root) for root in allowed):
        raise SourceError(
            "SOURCE_NOT_FOUND",
            f"source {resolved} is not under an allowed source root",
        )
    return resolved


def _is_under(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def ensure_regular_readable(path: Path) -> Path:
    try:
        is_file = path.is_file()
        readable = os.access(path, os.R_OK)
    except (OSError, ValueError) as err:
        raise SourceError("SOURCE_NOT_READABLE", f"cannot stat source: {path}") from err
    if not is_file:
        raise SourceError(
            "SOURCE_NOT_READABLE", f"source is not a regular file: {path}"
        )
    if not readable:
        raise SourceError("SOURCE_NOT_READABLE", f"source is not readable: {path}")
    return path


def validate_content_type(
    content_type: str, supported_formats: tuple[str, ...]
) -> None:
    if content_type not in supported_formats:
        raise SourceError(
            "UNSUPPORTED_FORMAT",
            f"unsupported content type {content_type!r}",
        )


def compute_sha256(path: Path, chunk_size: int = 1 << 20) -> str:
    """Streaming sha256 of a file, returned as ``sha256:<hex>``."""
    h = hashlib.sha256()
    try:
        with open(path, "rb") as f:
            while True:
                block = f.read(chunk_size)
                if not block:
                    break
                h.update(block)
    except (OSError, ValueError) as err:
        raise SourceError(
            "SOURCE_NOT_READABLE", f"failed to hash source: {path}"
        ) from err
    return f"sha256:{h.hexdigest()}"


def ensure_hash_matches(path: Path, expected_hash: str) -> None:
    """Compare ``content_hash`` (``sha256:<hex>`` unless prefixed) with the file."""
    if not expected_hash:
        return
    if ":" in expected_hash:
        _scheme, expected = expected_hash.split(":", 1)
    else:
        expected = expected_hash
    actual = compute_sha256(path)
    actual_hex = actual.split(":", 1)[1]
    if actual_hex != expected:
        raise SourceError(
            "SOURCE_HASH_MISMATCH",
            f"content hash mismatch: {actual} != {expected_hash}",
        )


def validate_source(
    local_path: str,
    content_type: str,
    content_hash: str,
    allowed_roots: tuple[str, ...],
    supported_formats: tuple[str, ...],
) -> Path:
    """Full source validation pipeline. Returns the validated readable path."""
    path = ensure_allowed_path(local_path, allowed_roots)
    validate_content_type(content_type, supported_formats)
    path = ensure_regular_readable(path)
    ensure_hash_matches(path, content_hash)
    return path
