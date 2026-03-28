# Context Window Overflow and Truncation

How AXIOM handles conversations and documents that exceed a model's context window, and what to do when things go wrong.

## How It Works

Every LLM has a maximum context window -- the total number of tokens it can process in a single request. When AXIOM assembles a prompt (system instructions, conversation history, research notes, and document content), the combined token count can exceed this limit.

AXIOM's `model_dispatcher` includes automatic overflow protection that operates in two stages:

### Stage 1: Pre-Request Truncation

Before sending any request to a provider, AXIOM estimates the token count and truncates the message history to **80% of the model's context limit**. This leaves headroom for the model's response and avoids hitting hard limits.

Token counts are estimated using a conservative formula:

```
estimated_tokens = len(text) / 3.2
```

!!! note "Why 3.2 instead of 4?"
    The commonly cited ratio of 1 token per 4 characters assumes English prose. AXIOM uses `len/3.2` because German compound words, technical terminology, and non-Latin scripts produce more tokens per character. This conservative estimate reduces the chance of underestimating and hitting the limit.

### Stage 2: Retry on 400 Error

If the provider still returns a `400` error with a message containing `"maximum context length"`, AXIOM:

1. Extracts the **actual context limit** from the error response.
2. Computes a **correction factor** based on the ratio of real token count to estimated token count.
3. Truncates the message to **75% of the real limit**, adjusted by the correction factor.
4. Retries the request **once**.

!!! warning
    The correction factor can be significant -- actual token counts may be **30-40% higher** than estimates, particularly for non-English text. The retry mechanism accounts for this discrepancy automatically.

### Provider Context Limits

Context window sizes are defined per-provider in the `PROVIDER_CONFIG` dictionary. Each provider and model combination has a known maximum. AXIOM uses these values for pre-request truncation. If a model is not in the config, AXIOM falls back to a conservative default.

## What Users See

When context overflow handling activates, AXIOM logs warnings that appear in the backend logs:

- **Pre-truncation**: A log entry indicates that messages were truncated to fit the context window, including the estimated token count and the target limit.
- **Retry on error**: A warning is logged showing the original error, the correction factor applied, and the adjusted token budget for the retry.

Users do not see these warnings in the frontend UI. If both stages fail, the request returns an error that surfaces as a failed agent step in the mission log.

## Troubleshooting Persistent Overflow

If you see repeated context overflow errors in the logs, work through these steps:

### 1. Check the Model's Context Window

Verify that the model you are using has a sufficiently large context window for your workload.

```bash
docker compose logs axiom-backend | grep -i "context\|truncat\|overflow"
```

### 2. Reduce Input Size

- **Shorter documents**: Break large documents into smaller chunks before uploading.
- **Fewer research iterations**: Reduce the number of reflection loops in mission settings to limit accumulated context.
- **Concise mission prompts**: Avoid very long custom instructions in the mission description.

### 3. Switch to a Larger Context Model

Some models support significantly larger context windows:

| Provider   | Model Example          | Context Window |
|------------|------------------------|----------------|
| OpenAI     | gpt-4o                 | 128k tokens    |
| OpenRouter | anthropic/claude-3.5   | 200k tokens    |
| DeepSeek   | deepseek-chat          | 64k tokens     |

!!! tip
    For research missions that process many documents or require deep multi-step analysis, prefer models with at least 128k context windows. Configure this in **Settings > AI Config**.

### 4. Inspect the Correction Factor

If the logs show a high correction factor (above 1.3), your content likely contains a high density of non-English or technical text. This is expected behavior -- AXIOM compensates automatically on retry. No action is needed unless retries also fail.

### 5. Check Provider-Specific Limits

Some providers enforce limits that differ from advertised context windows. If you use a custom or self-hosted provider, verify that the context limit in `PROVIDER_CONFIG` matches the actual capability of your deployment.

## Summary

| Stage            | Trigger                        | Action                                      |
|------------------|--------------------------------|---------------------------------------------|
| Pre-truncation   | Estimated tokens > 80% limit   | Truncate messages before sending            |
| Error retry      | 400 "maximum context length"   | Extract real limit, apply correction, retry |
| Final failure    | Retry also fails               | Error surfaces in mission log               |

The system is designed to handle overflow transparently in most cases. Manual intervention is only needed when content consistently exceeds even the retry budget.
