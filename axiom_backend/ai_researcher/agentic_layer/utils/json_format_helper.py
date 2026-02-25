"""
JSON Format Helper with Automatic Fallback

This module provides utilities to handle JSON response formats with automatic
fallback from json_schema to json_object format when needed.
"""

from typing import Dict, Any, Optional, List, Tuple
from pydantic import BaseModel
import json
import logging

logger = logging.getLogger(__name__)

# Runtime cache: tracks the best format mode per (provider, model) pair.
# Populated dynamically when json_schema or json_object calls fail.
# Key: (provider, model) tuple — Value: "json_object" or "none"
_format_fallback_cache: Dict[Tuple[str, str], str] = {}


def mark_format_unsupported(provider: str, model: str, failed_mode: str) -> None:
    """
    Record that a (provider, model) pair does not support a given format mode.
    Called after a json_schema or json_object call fails, so subsequent calls
    skip directly to the working mode.
    """
    key = (provider.lower(), model.lower())
    if failed_mode == "json_schema":
        fallback = "json_object"
    elif failed_mode == "json_object":
        fallback = "none"
    else:
        return
    _format_fallback_cache[key] = fallback
    logger.info(
        f"Cached format fallback: ({provider}, {model}) -> skip to '{fallback}'"
    )


def get_initial_format_mode(provider: Optional[str] = None, model: Optional[str] = None) -> str:
    """
    Return the best initial response format mode for a (provider, model) pair.

    Uses a runtime cache: the first call for a new pair tries json_schema.
    If it fails, mark_format_unsupported() caches the result so all subsequent
    calls skip directly to the working mode.
    """
    if provider and model:
        key = (provider.lower(), model.lower())
        cached = _format_fallback_cache.get(key)
        if cached:
            logger.info(
                f"Using cached format mode '{cached}' for ({provider}, {model})"
            )
            return cached
    return "json_schema"


def _make_strict_compatible(schema: Dict[str, Any]) -> Dict[str, Any]:
    """
    Post-process a Pydantic-generated JSON schema to be compatible with
    OpenAI's strict structured outputs mode.

    Strict mode requires:
    - All properties listed in 'required'
    - No 'default' values
    - 'additionalProperties': false on all objects

    This recursively processes the schema and all $defs.
    """

    def _fix_object(obj: Dict[str, Any]) -> None:
        if not isinstance(obj, dict):
            return

        # Fix object types: ensure all properties are in required, remove defaults
        if obj.get("type") == "object" and "properties" in obj:
            obj["required"] = list(obj["properties"].keys())
            obj["additionalProperties"] = False
            for prop in obj["properties"].values():
                prop.pop("default", None)
                _fix_object(prop)

        # Recurse into anyOf / oneOf / allOf
        for key in ("anyOf", "oneOf", "allOf"):
            if key in obj:
                for item in obj[key]:
                    _fix_object(item)

        # Recurse into items (arrays)
        if "items" in obj and isinstance(obj["items"], dict):
            _fix_object(obj["items"])

    schema = json.loads(json.dumps(schema))  # deep copy
    _fix_object(schema)

    # Process $defs (nested model definitions)
    for def_schema in schema.get("$defs", {}).values():
        _fix_object(def_schema)

    return schema


def get_json_schema_format(
    pydantic_model: type[BaseModel], schema_name: str = "response"
) -> Dict[str, Any]:
    """
    Get json_schema format configuration (OpenAI structured outputs).

    Args:
        pydantic_model: The Pydantic model class defining the schema
        schema_name: A descriptive name for the schema

    Returns:
        Dictionary with json_schema format configuration
    """
    raw_schema = pydantic_model.model_json_schema()
    strict_schema = _make_strict_compatible(raw_schema)

    return {
        "type": "json_schema",
        "json_schema": {
            "name": schema_name,
            "schema": strict_schema,
            "strict": True,  # Enable strict validation
        },
    }


def get_json_object_format() -> Dict[str, Any]:
    """
    Get json_object format configuration (basic JSON mode).

    Returns:
        Dictionary with json_object format configuration
    """
    return {"type": "json_object"}


def get_schema_instructions(pydantic_model: type[BaseModel]) -> str:
    """
    Generate clear schema instructions for models using json_object format.

    Args:
        pydantic_model: The Pydantic model class defining the schema

    Returns:
        String containing formatted schema instructions
    """
    schema = pydantic_model.model_json_schema()

    # Create a simplified, human-readable version of the schema
    instructions = "\n\nIMPORTANT: You must respond with a JSON object that EXACTLY follows this schema:\n"
    instructions += json.dumps(schema, indent=2)
    instructions += "\n\nAll required fields must be included. Use empty arrays [] for list fields with no items, and null for optional fields."

    return instructions


def enhance_messages_for_json_object(
    messages: List[Dict[str, str]], pydantic_model: type[BaseModel]
) -> List[Dict[str, str]]:
    """
    Enhance messages with schema instructions for json_object format.

    Args:
        messages: List of message dictionaries
        pydantic_model: The Pydantic model class defining the schema

    Returns:
        Enhanced messages list
    """
    # Make a copy to avoid modifying the original
    enhanced_messages = [msg.copy() for msg in messages]

    # Add schema instructions to the user message
    if enhanced_messages and enhanced_messages[-1]["role"] == "user":
        schema_instructions = get_schema_instructions(pydantic_model)
        enhanced_messages[-1]["content"] += schema_instructions

    # Enhance system prompt for better JSON compliance
    for i, msg in enumerate(enhanced_messages):
        if msg["role"] == "system":
            enhanced_messages[i]["content"] += (
                "\n\nYou are a JSON-only assistant. Always respond with valid JSON matching the exact schema provided."
            )
            break

    return enhanced_messages


def get_response_formats_with_fallback(
    pydantic_model: type[BaseModel], schema_name: str = "response"
) -> Tuple[Dict[str, Any], Dict[str, Any]]:
    """
    Get both json_schema and json_object formats for fallback strategy.

    Args:
        pydantic_model: The Pydantic model class defining the schema
        schema_name: A descriptive name for the schema

    Returns:
        Tuple of (json_schema_format, json_object_format)
    """
    json_schema_format = get_json_schema_format(pydantic_model, schema_name)
    json_object_format = get_json_object_format()

    return json_schema_format, json_object_format


def should_retry_with_json_object(error: Exception) -> bool:
    """
    Determine if an error indicates we should retry with json_object format.

    Args:
        error: The exception that occurred

    Returns:
        True if we should retry with json_object format
    """
    error_str = str(error).lower()

    exclusion_patterns = [
        "context_length_exceeded",
        "maximum context length",
        "reduce the length",
        "token limit",
        "too many tokens",
        "rate limit",
        "rate_limit",
        "insufficient_quota",
        "billing",
        "api key",
        "authentication",
        "permission denied",
        "model not found",
        "invalid api key",
    ]

    for pattern in exclusion_patterns:
        if pattern in error_str:
            return False

    json_schema_error_patterns = [
        "json_schema",
        "text.format",
        "response_format type is unavailable",
        "not supported",
        "invalid parameter",
        "unsupported format",
        "moonshot flavored json schema",
        "invalid moonshot",
        "keyword 'default' is not allowed",
        "not a valid moonshot",
        "structured outputs are not supported",
        "json_object' is required",
        "error resolving schema reference",
        "recursionerror",
        "maximum recursion depth exceeded",
        "provider returned error",
        "invalid schema for response_format",
        "'required' is required to be supplied",
    ]

    for pattern in json_schema_error_patterns:
        if pattern in error_str:
            logger.info(
                f"Detected json_schema compatibility issue: {error_str[:200]}..."
            )
            return True

    return False


def should_retry_without_response_format(error: Exception) -> bool:
    """
    Determine if an error indicates we should retry without any response_format.
    This is for providers like DeepSeek that don't support any structured output format.

    Args:
        error: The exception that occurred

    Returns:
        True if we should retry without response_format
    """
    error_str = str(error).lower()

    exclusion_patterns = [
        "context_length_exceeded",
        "maximum context length",
        "reduce the length",
        "token limit",
        "too many tokens",
        "rate limit",
        "rate_limit",
        "insufficient_quota",
        "billing",
        "api key",
        "authentication",
        "permission denied",
        "model not found",
        "invalid api key",
    ]

    for pattern in exclusion_patterns:
        if pattern in error_str:
            return False

    no_format_error_patterns = [
        "this response_format type is unavailable",
        "response_format type is unavailable",
        "response_format is not supported",
    ]

    for pattern in no_format_error_patterns:
        if pattern in error_str:
            logger.info(f"Detected response_format not supported: {error_str[:200]}...")
            return True

    return False
