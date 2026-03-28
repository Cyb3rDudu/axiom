# Multilingual Research

AXIOM supports conducting research missions in multiple languages. This page explains how to configure language settings and what they affect throughout the research pipeline.

## Setting the Language

Each research mission has a **language code** that determines how agents communicate, write, and format citations.

To set the language:

1. Open or create a research mission.
2. Go to **Mission Settings**.
3. Select the desired language from the language dropdown.
4. Save the settings.

The language code (e.g., `de` for German, `en` for English) is stored with the mission and passed to every agent in the pipeline.

!!! note
    Language is set per-mission, not globally. You can run multiple missions in different languages simultaneously.

## What Language Affects

### Agent Behavior

The `language_code` is propagated to all agents in the research pipeline:

- **Planning Agent** -- generates the research plan and outline in the target language.
- **Research Agent** -- conducts searches and synthesizes findings using the target language.
- **Reflection Agent** -- critiques research quality and identifies gaps, writing feedback in the target language.
- **Note Assignment Agent** -- organizes and assigns notes with language-appropriate categorization.
- **Writing Agent** -- drafts the final report sections in the target language.
- **Writing Reflection Agent** -- reviews drafts for clarity and coherence in the target language.
- **Messenger Agent** -- communicates status updates and summaries in the target language.

Each agent receives the language code as part of its configuration, ensuring consistent language use across all phases of the mission.

### Prompt Selection

Prompts sent to the LLM can be language-specific. When a language-specific prompt variant exists, AXIOM uses it. Otherwise, prompts default to English with an instruction to respond in the target language.

### Citation Formatting

Citation profiles are language-aware. AXIOM includes built-in profiles such as:

- **German KMU APA 7** -- APA 7th edition adapted for German academic conventions.
- **English APA 7** -- Standard APA 7th edition in English.

The citation profile is selected in mission settings and works in conjunction with the language code to produce properly formatted references.

!!! tip
    When working in German, select both the German language code and a German-specific citation profile. Mixing a German language code with an English citation profile will produce a report in German but with English-formatted references.

## Supported Languages

AXIOM's language support depends on the underlying LLM's capabilities. Most modern models handle the following languages well:

| Language | Code | Notes |
|----------|------|-------|
| English  | `en` | Default; best supported across all providers |
| German   | `de` | Fully tested; token estimation tuned for German |
| French   | `fr` | Supported by most providers |
| Spanish  | `es` | Supported by most providers |
| Other    | varies | Depends on the model's training data |

!!! warning
    Languages with non-Latin scripts (Chinese, Japanese, Arabic, etc.) work but may require models with strong multilingual capabilities. Token estimation accuracy decreases for these scripts, which can affect context window calculations.

## Token Estimation and Non-English Text

AXIOM uses a conservative token estimation formula (`len/3.2` instead of the typical `len/4`) specifically because of multilingual support. This matters because:

- **German compound words** (e.g., "Forschungsergebniszusammenfassung") tokenize into more tokens than their character count would suggest at a `len/4` ratio.
- **Technical terminology** in any language tends to produce more tokens per character.
- **Non-Latin scripts** (CJK, Cyrillic, Arabic) have a much higher token-to-character ratio.

The conservative estimate helps prevent context window overflow when working in these languages. See [Context Window Overflow](../../troubleshooting/context-overflow.md) for details on how AXIOM handles overflow situations.

## Best Practices

1. **Match language and citation profile** -- always select a citation profile that matches your mission language.
2. **Use capable models** -- for non-English research, prefer models known for strong multilingual performance (GPT-4o, Claude, Qwen).
3. **Keep prompts in the target language** -- while AXIOM translates its internal prompts, custom mission instructions in the target language produce better results.
4. **Test with a short mission first** -- when working in a new language, run a short test mission to verify output quality before committing to a full research run.
5. **Allow headroom for context** -- non-English text consumes more tokens. Use models with larger context windows or keep document sets smaller when working in token-heavy languages.
