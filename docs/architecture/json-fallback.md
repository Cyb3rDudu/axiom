# JSON Response Format Fallback System

AXIOM supports multiple LLM providers (OpenAI, OpenRouter, DeepSeek, custom endpoints), and each handles structured JSON output differently. The JSON fallback system ensures that every agent can extract structured data from any provider, regardless of its JSON capabilities.

## The Problem

AXIOM's agents -- planning, research, writing, reflection, note assignment, messenger, writing reflection, and collaborative writing -- all require structured JSON responses to function. However:

- Some providers support **strict JSON schema** enforcement (OpenAI).
- Some support **JSON mode** but not schema validation.
- Some support **neither** (DeepSeek reasoner models, older endpoints).

A single hardcoded approach would lock AXIOM into one provider tier. The fallback system solves this by trying progressively simpler JSON strategies until one works.

## The Three-Level Fallback Chain

```
Request with json_schema
        |
        v
   Success? -----> Yes: Use response, cache "json_schema" for this (provider, model)
        |
        No (schema error)
        |
        v
Request with json_object
        |
        v
   Success? -----> Yes: Use response, cache "json_object" for this (provider, model)
        |
        No (format error)
        |
        v
Request with prompt-only
        |
        v
   Parse JSON from raw text, cache "prompt_only" for this (provider, model)
```

### Level 1: `json_schema` (Structured Output)

The strongest guarantee. AXIOM sends a Pydantic model as the `response_format` parameter, and the provider enforces that the response conforms to the schema.

- **Used by**: OpenAI, compatible providers that support the `json_schema` response format.
- **Behavior**: The LLM is constrained to output valid JSON matching the exact field names, types, and structure defined in the Pydantic model.
- **Advantage**: No post-processing or validation needed. Responses are guaranteed to parse correctly.

### Level 2: `json_object` (JSON Mode)

A weaker guarantee. AXIOM requests JSON output without specifying a schema. The response is valid JSON but may not match the expected structure exactly.

- **Used by**: Providers that support `{"type": "json_object"}` but reject schema definitions.
- **Behavior**: The LLM outputs valid JSON. AXIOM parses it and maps fields to the expected Pydantic model with best-effort matching.
- **Advantage**: Still guarantees parseable JSON, reducing extraction errors.

### Level 3: Prompt-Only (No Response Format)

The fallback of last resort. No `response_format` parameter is sent. Instead, the prompt itself instructs the LLM to respond in JSON, and AXIOM extracts JSON from the raw text response.

- **Used by**: DeepSeek reasoner models, providers with no JSON format support.
- **Behavior**: The LLM receives prompt instructions like "Respond with a JSON object containing the following fields..." AXIOM then parses the response, handling markdown code fences and surrounding text.
- **Advantage**: Works with any provider, regardless of feature support.

!!! warning
    Level 3 is the least reliable. Responses may include explanatory text around the JSON, malformed fields, or missing keys. AXIOM includes parsing logic to handle common issues, but occasional failures are expected with less capable models.

## Dynamic Runtime Cache

The fallback level is **not configured statically**. Instead, AXIOM discovers the right level at runtime:

1. The first request for a given `(provider, model)` pair starts at Level 1.
2. If the request fails with a **schema-related error**, AXIOM falls back to Level 2, then Level 3.
3. The working level is **cached in memory** for the `(provider, model)` pair.
4. All subsequent requests for that pair skip directly to the cached level.

This means:

- There is a one-time discovery cost per provider/model combination per process lifetime.
- Switching models or providers triggers a new discovery sequence.
- The cache resets on backend restart.

!!! note
    The system distinguishes schema/format errors from unrelated errors (context overflow, rate limits, authentication failures). Only format-related failures trigger fallback. Other errors are raised normally.

## Error Pattern Matching

AXIOM inspects error responses to determine whether fallback is appropriate:

| Error Type | Triggers Fallback? | Reason |
|------------|-------------------|--------|
| Schema validation rejected | Yes | Provider does not support `json_schema` |
| `json_object` format rejected | Yes | Provider does not support JSON mode |
| Context length exceeded | No | Handled by [context overflow logic](../troubleshooting/context-overflow.md) |
| Rate limit exceeded | No | Transient error, should be retried |
| Authentication failed | No | Configuration issue, not format-related |

## Why This Matters

The fallback system is what makes AXIOM provider-agnostic. Without it, users would need to manually configure JSON compatibility per model, or be restricted to a single provider. With the fallback:

- **New providers work automatically** -- no configuration needed for JSON support level.
- **Mixed deployments are seamless** -- use OpenAI for planning (Level 1) and DeepSeek for writing (Level 3) in the same mission.
- **Degradation is graceful** -- if a provider drops schema support, AXIOM adapts at runtime without downtime.
- **All agents benefit** -- the fallback applies uniformly across every agent type in the pipeline.
