# AXIOM Documentation Style Guide

Internal reference for writing documentation pages that are consistent with the existing AXIOM docs.

---

## 1. Heading Hierarchy

### Rules

- **H1 (`#`)** -- One per file. Always the first line. States what the page covers in plain terms. No articles ("the", "a") unless needed for grammar. Examples: `# AI Provider Configuration`, `# Document Groups`, `# Linux Installation`.
- **H2 (`##`)** -- Major sections that appear in the right-hand table of contents. Used for top-level groupings such as `## Overview`, `## Prerequisites`, `## Troubleshooting`, `## Next Steps`.
- **H3 (`###`)** -- Sub-topics within an H2. Named with the specific item: `### Using Ollama`, `### Fast Model`, `### Step 1: Clone Repository`.
- **H4 (`####`)** -- Rare. Only used inside an H3 when a further breakdown is necessary, such as numbered sub-tabs (`#### 1. Plan Tab`) or sub-categories (`#### Worker Thread Configuration`).

### Observed Pattern

```
# Page Title                          (one per file)
## Major Section                      (Overview, Prerequisites, Configuration, ...)
### Specific Topic                    (concrete item, provider, step)
#### Sub-detail                       (only when H3 is already dense)
```

Never skip a level (no H1 followed immediately by H3).


## 2. Page Structure (Feature Pages)

Feature documentation follows a predictable arc. Not every section is required, but the order is fixed.

| Order | Section             | Purpose                                         |
|-------|---------------------|-------------------------------------------------|
| 1     | H1 Title            | Name of the feature                             |
| 2     | One-line summary    | Single sentence below H1 explaining the page    |
| 3     | Screenshot          | Hero image of the feature's UI (when available) |
| 4     | `## Overview` / `## How ... Works` | Conceptual explanation         |
| 5     | `## Getting Started` / `## Creating ...` | First-use walkthrough   |
| 6     | `## Configuration`  | Knobs and parameters                            |
| 7     | `## Advanced Features` / `## Best Practices` | Power-user guidance  |
| 8     | `## Troubleshooting` | Problem/solution pairs                          |
| 9     | `## Next Steps`     | Numbered or bulleted list of related links       |

Example from `ai-providers.md`:

```markdown
# AI Provider Configuration

Configure language model providers to power AXIOM's research and writing capabilities.

![AI Configuration Interface](../../assets/images/settings/ai-config.png)

## Overview
...
## Supported Providers
...
## Configuration Mode
...
## Best Practices
...
## Troubleshooting
...
## Next Steps
```


## 3. Admonition Usage

AXIOM uses Material for MkDocs admonitions via `pymdownx.details` and the `admonition` extension.

### Syntax

```markdown
!!! type "Optional custom title"
    Content indented by 4 spaces.
    Multiple lines are fine.
```

### Which Type to Use

| Type          | When to use                                                    | Example context                                |
|---------------|----------------------------------------------------------------|------------------------------------------------|
| `tip`         | Helpful suggestion the reader may not think of                 | "Start with the default (20) and adjust..."    |
| `info`        | Supplemental explanation or clarification                      | "Concurrency Layers Explained"                 |
| `important`   | Prerequisite or hard requirement the reader must not skip      | "NVIDIA Driver Requirement for GPU Users"      |
| `warning`     | Something that can break if ignored; data-loss risk            | "Driver Version Requirement"                   |
| `success`     | Positive confirmation or a simplified approach that works well | "Windows Installation Simplified"              |
| `note`        | Minor aside, less prominent than `info`                        | Quick clarification about URL formats          |

### Patterns Observed

- **Page-level callouts** appear immediately below the H1 title using `!!! important` for hard prerequisites.
- **Inline callouts** are placed inside a section, typically after a code block or numbered list to explain a nuance.
- Nested admonitions are used sparingly but do appear (e.g., `!!! info` nested inside a configuration section).
- Custom titles are always in double quotes: `!!! warning "Driver Version Requirement"`.

### Blockquote Notes (Legacy)

Some older pages use `> **Note**: ...` blockquote syntax for quick notes. New pages should prefer `!!! note` admonitions for consistency, but do not rewrite legacy pages unless already editing them.


## 4. Code Block Patterns

### Language Tags

| Language | When used                                          |
|----------|----------------------------------------------------|
| `bash`   | Shell commands, Docker commands, `.env` snippets   |
| `python` | Python code, backend debugging commands            |
| `yaml`   | `docker-compose.yml` snippets, MkDocs config       |
| (none)   | Plain text examples like user chat messages         |

### Formatting Rules

- Always specify the language tag after the triple backticks.
- For shell commands, do NOT include a `$` prompt prefix. Just write the command directly.
- Multi-step shell sessions use comments (`#`) to label each command:

```bash
# Install Ollama
curl -fsSL https://ollama.com/install.sh | sh

# Pull a model
ollama pull llama2

# Run Ollama server
ollama serve
```

- Inline code uses single backticks for: file paths, environment variable names, model names, URLs, UI labels when mixed into prose. Example: `http://host.docker.internal:11434/v1/`.
- Configuration examples use `bash` even for `.env` files:

```bash
# In .env file
MAX_WORKER_THREADS=20
```

- YAML snippets always carry a comment indicating the source file:

```yaml
# In docker-compose.yml
volumes:
  - ./axiom_model_cache:/root/.cache/huggingface
```


## 5. Table Formatting

Tables use the standard GitHub-flavored Markdown pipe syntax with header separators.

### Rules

- Left-align all columns (no `:---:` centering) unless the data is clearly numeric.
- Keep cell content concise -- one phrase or short sentence per cell.
- Use bold for column headers.
- Common table patterns: comparison tables, error-reference tables, parameter tables.

### Examples

**Comparison table** (`ai-providers.md`):

```markdown
| Provider | Pros | Cons | Best For |
|----------|------|------|----------|
| OpenRouter | 100+ models, unified billing | Adds small overhead | Flexibility |
| OpenAI | Direct access, latest models | Single vendor | GPT users |
| Local LLMs | Privacy, no costs | Requires hardware | Sensitive data |
```

**Error reference table** (`ai-models.md`):

```markdown
| Error | Solution |
|-------|----------|
| "API key invalid" | Check key in Settings -> AI Config |
| "Model not found" | Use full model path (provider/model) |
```


## 6. Link Formatting

### Relative Paths

All internal links use relative paths from the current file's location.

```markdown
<!-- Same directory -->
[Search Providers](search-providers.md)

<!-- Parent directory -->
[First Login](../first-login.md)

<!-- Different section -->
[Documents Overview](../../user-guide/documents/overview.md)
```

### Cross-References with Anchors

Link to a specific section by appending `#anchor`:

```markdown
[Cost Tracking Discrepancies](../../troubleshooting/common-issues/ai-models.md#cost-tracking-discrepancies)
```

### External Links

External URLs always include descriptive text:

```markdown
[OpenRouter Dashboard](https://openrouter.ai/keys)
[Community Forum](https://github.com/murtaza-nasir/axiom/discussions)
```

### "Learn More" Pattern

Settings overview pages use an arrow suffix for sub-page links:

```markdown
[Learn more about Profile Settings ->](profile.md)
```


## 7. Grid Cards

Grid cards use the Material for MkDocs `grid cards` feature, enabled by the `attr_list` and `md_in_html` extensions.

### Simple Cards (Navigation)

Used on index pages and overview pages to provide navigation links.

```markdown
<div class="grid cards" markdown>

-   **[Quick Start](getting-started/quickstart.md)**

    Get AXIOM up and running in minutes with Docker

-   **[Installation](getting-started/installation/index.md)**

    Detailed installation instructions for various platforms

</div>
```

Rules:
- Each card starts with `-   ` (3 spaces after the dash).
- Title is bold and linked: `**[Title](path.md)**`.
- A blank line separates the title from the description.
- Description is a single plain sentence with no period.

### Feature Cards (with Icons and Dividers)

Used for feature showcases on landing pages.

```markdown
<div class="grid cards" markdown>

-   :material-file-document-multiple: **Document Management**

    ---

    Upload and manage your research documents in a central library

    - PDF, Word, and Markdown support
    - Advanced RAG pipeline with BGE-M3 embeddings
    - Semantic search across all documents

</div>
```

Rules:
- Icon prefix uses Material Design icon syntax: `:material-icon-name:`.
- `---` horizontal rule separates the title from the body.
- Body can contain a brief description followed by a bullet list.

### Numbered Cards

Used for sequential steps on index pages.

```markdown
<div class="grid cards" markdown>

-   :material-numeric-1-circle: **[First Login](../getting-started/first-login.md)**

    Set up your account and change the default password

-   :material-numeric-2-circle: **[Upload Documents](documents/uploading.md)**

    Build your document library for research

</div>
```

### Plain Grid (No Cards)

Used for side-by-side content blocks without card styling.

```markdown
<div class="grid" markdown>

:material-brain: **Advanced AI Integration**
Leverage multiple LLMs for diverse research capabilities

:material-magnify: **Deep Document Analysis**
Advanced RAG pipeline with dual embeddings for superior search accuracy

</div>
```


## 8. Image Reference Patterns

### Syntax

```markdown
![Alt text description](../../assets/images/subfolder/image-name.png)
```

### Rules

- All images live under `docs/assets/images/`.
- Subdirectories mirror the feature area: `settings/`, `troubleshooting/`, or top-level.
- Alt text is a human-readable description of the screenshot, not a filename.
- Hero screenshots appear immediately after the one-line summary, before `## Overview`.
- In-section screenshots appear at the start of their relevant section.
- File names may contain spaces (legacy convention): `Research view.png`, `doc view document groups.png`. New images should prefer hyphens: `research-report-main.png`.

### Centered Images with Styling (Landing Pages Only)

```html
<div align="center" style="margin: 2rem 0;">
  <img src="assets/images/research-report-main.png" alt="Research Report Example"
       style="max-width: 100%; border-radius: 8px; box-shadow: 0 4px 6px rgba(0,0,0,0.1);"/>
</div>
```

This HTML pattern is only used on the home page (`index.md`). All other pages use standard Markdown image syntax.


## 9. Step-by-Step Instructions

### Numbered Steps

Used for multi-action procedures. Each step starts with a number and a bold action or label.

```markdown
### Step 1: Clone Repository

\```bash
git clone https://github.com/murtaza-nasir/axiom.git
cd axiom
\```

### Step 2: Configure Environment

Use the interactive setup script:

\```bash
./setup-env.sh
\```

The script will guide you through:

1. **Network Configuration**
    - Simple (localhost only)
    - Network (LAN access)
    - Custom domain

2. **Security Configuration**
    - Generates secure passwords automatically
    - Sets up JWT secrets
```

### Inline Numbered Lists

For shorter procedures within a section:

```markdown
**Setup:**

1. Select "OpenAI" as the AI Provider
2. API Key: Get from [OpenAI Platform](https://platform.openai.com/api-keys)
3. Base URL: `https://api.openai.com/v1/`
4. Click "Test" to verify and load models
```

### Rules

- Use `### Step N: Action` headings for major multi-step procedures (installation, initial setup).
- Use plain numbered lists for shorter in-section procedures (3-6 steps).
- Bold UI element names inside steps: **"Save & Close"**, **Settings**, **AI Config**.
- Include the purpose/result after a step when it is not obvious.


## 10. Tone and Voice

### Person

Second person ("you", "your") throughout. Never "we" when referring to the reader. "We" is acceptable only when referring to AXIOM's team or software behavior: "We provide a test script to verify pricing discrepancies."

### Register

Semi-formal and direct. The docs read like a knowledgeable colleague explaining the system -- not an academic paper and not casual chat.

- Prefer short, declarative sentences.
- Use imperative mood for instructions: "Click the Test button", "Enter your API key".
- Avoid hedging: say "This requires X" not "You might need X".
- Avoid filler: no "simply", "just", "easily", "please".

### Technical Terms

- Capitalize product names: AXIOM, Docker, PostgreSQL, OpenRouter.
- Model names appear in code font when used precisely: `gpt-5-chat`, `claude-3.5-haiku`.
- UI elements are bold: **Save & Close**, **AI Config**, **Test**.
- Environment variables are in code font: `MAX_WORKER_THREADS`, `AXIOM_PORT`.

### Emotional Markers

The docs do not use emojis in body text. Emojis appear only in:
- Material Design icon references (`:material-rocket:`) inside grid cards.
- Warning symbols in `.env` code comments (`# <warning symbol> CHANGE THIS!`) which are legacy and should not be added to new pages.


## 11. Section Endings -- Next Steps

Almost every page ends with a `## Next Steps` section. This provides 3-5 related pages the reader is likely to visit next.

### Format

Bulleted list with relative links and a brief dash description:

```markdown
## Next Steps

- [Configure AI Providers](../configuration/ai-providers.md) - Set up your language models
- [Setup Document Processing](../../user-guide/documents/overview.md) - Manage your document library
- [Configure Search Providers](../configuration/search-providers.md) - Enable web search
```

Or numbered list for a recommended sequential flow:

```markdown
## Next Steps

1. [Profile Settings](profile.md) - Set up your account
2. [AI Configuration](ai-config.md) - Configure language models
3. [Search Settings](search-config.md) - Enable web search
4. [Research Settings](research-config.md) - Optimize research missions
```

### Rules

- Use a numbered list when the order matters (onboarding flows).
- Use a bulleted list when the links are peer options.
- Each item: `[Link Text](relative-path.md)` followed by ` - ` and a brief clause.
- Always provide at least 3 links.


## 12. Navigation Structure (mkdocs.yml)

### Hierarchy

The nav uses a three-level hierarchy:

```yaml
nav:
  - Top Section:
    - Sub-section:
      - page-path.md
```

### Conventions

- Top-level sections use title case: `Getting Started`, `User Guide`, `Troubleshooting`.
- Sub-sections use title case: `Common Issues`, `By Model`.
- Index pages use the bare path without a label to become the section landing page:
  ```yaml
  - Installation:
    - getting-started/installation/index.md   # section index (no label)
    - Linux: getting-started/installation/linux.md
  ```
- Leaf pages have an explicit label: `- Linux: getting-started/installation/linux.md`.
- File paths are always relative to the `docs/` directory.

### Where to Place New Pages

| Page type                 | Location                                            |
|---------------------------|-----------------------------------------------------|
| New setting tab           | `user-guide/settings/<tab-name>.md`                 |
| New feature guide         | `user-guide/<feature>/overview.md`                  |
| New configuration topic   | `getting-started/configuration/<topic>.md`           |
| New troubleshooting page  | `troubleshooting/common-issues/<topic>.md`           |
| New installation variant  | `getting-started/installation/<variant>.md`           |


## 13. Troubleshooting Sections

Troubleshooting appears both as standalone pages and as sections within feature docs.

### Inline Troubleshooting (End of Feature Page)

```markdown
## Troubleshooting

### Problem Title

**Error:** Description of what the user sees

**Solution:**

1. First fix step
2. Second fix step

\```bash
# Diagnostic command
docker compose logs axiom-backend
\```
```

### Standalone Troubleshooting Pages

Follow the pattern from `ai-models.md`:

```markdown
# AI Model Troubleshooting

Quick fixes for AI model configuration and API issues.

## Configuration Issues
### No Models in Dropdown
**Solution:**
1. ...

## API Issues
### Authentication Failed
**Error:** Invalid API key
**Solution:**
\```bash
curl ...
\```
```

### Common Error Table

Often appears at the bottom of a troubleshooting page:

```markdown
## Common Error Messages

| Error | Solution |
|-------|----------|
| "API key invalid" | Check key in Settings -> AI Config |
| "Model not found" | Use full model path (provider/model) |
```


## 14. Miscellaneous Conventions

- **Horizontal rules**: `---` used sparingly, mainly on landing pages to separate major visual sections.
- **Bold labels**: Configuration fields and UI labels are bold. Example: `**API Key**: Enter your API key`.
- **Parenthetical clarifications**: Used for secondary info: `(5-10 minutes)`, `(admin users only)`.
- **"Configuration:" / "Setup:" headers**: Bold text followed by a colon introduces a short parameter list. Not a heading.
- **Bullet list style**: Dashes (`-`) for unordered lists, not asterisks.
- **Indentation**: 4 spaces for sub-items under numbered lists, 4 spaces for admonition content.
- **Mermaid diagrams**: Available via `pymdownx.superfences`. Used on the home page for architecture. Use sparingly on internal pages.
- **No trailing whitespace** in files; no blank lines at end of file.
