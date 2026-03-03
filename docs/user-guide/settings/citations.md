# Citation Settings

Manage citation profiles that control how the writing agent formats in-text citations and bibliographies in your research reports.

## Overview

Citation profiles define the rules that AXIOM's writing agent follows when inserting references into generated text and compiling bibliographies. You can use the built-in profiles, create custom ones, and set defaults at both the user and mission level.

## Built-in Profiles

AXIOM ships with three built-in citation profiles. These profiles cannot be edited or deleted.

| Profile | Mode | Language | Description |
|---------|------|----------|-------------|
| **Numbered Citations** | Numbered | English | Uses `[doc_id]` markers that are post-processed into sequential `[1]`, `[2]` references. This is the system default. |
| **KMU Akademie APA 6 (Deutsch)** | Author-Year | German | Follows APA 6th Edition conventions in German with `(Autor, Jahr, S. XX)` format. |
| **APA 7th Edition (English)** | Author-Year | English | Follows APA 7th Edition conventions in English with `(Author, Year, p. XX)` format. |

### Citation Modes

Each profile operates in one of two citation modes:

**Numbered Mode**

- The writing agent inserts raw `[doc_id]` placeholders (e.g., `[f28769c8]`) into the text as it writes.
- After the report is complete, AXIOM's post-processing step replaces these placeholders with sequential numbers (`[1]`, `[2]`, etc.) and generates a matching numbered bibliography.
- Best suited for technical reports and documents where concise numeric references are preferred.

**Author-Year Mode**

- The writing agent inserts `(Author, Year, p. XX)` citations directly into the text as it writes.
- No post-processing is applied; the citations appear in the final report exactly as written.
- Best suited for academic papers and formal publications that require author-year referencing.

## Setting a Default Profile

To set a default citation profile for all new missions:

1. Navigate to **Settings > Citations**
2. Locate the profile you want to use as your default
3. Click the **star icon** next to that profile
4. Click **Save & Close**

The starred profile is applied automatically to any new research or writing session unless overridden at the mission level.

!!! tip
    If no default is set, AXIOM falls back to the **Numbered Citations** profile.

## Per-Mission Override

You can override the default citation profile for a specific mission:

1. Open **Mission Settings** for the mission (gear icon)
2. Select a profile from the **Citation Profile** dropdown
3. The selected profile applies only to that mission

This is useful when different missions require different citation styles, for example a numbered style for an internal report and APA 7 for an academic paper.

## Quick-Select in Chat

During a writing session you can also change the active citation profile from the chat panel:

1. Click the **Quote** dropdown in the chat toolbar
2. Select the desired citation profile from the list
3. The writing agent uses the selected profile for subsequent responses

!!! note
    Changing the profile mid-session affects only new content generated after the change. Previously written text retains its original citation format.

## Creating Custom Profiles

If the built-in profiles do not match your requirements, you can create a custom citation profile:

1. Navigate to **Settings > Citations**
2. Click **New Profile**
3. Fill in the required fields:

| Field | Description |
|-------|-------------|
| **Profile ID** | A unique slug identifier (lowercase letters, numbers, and underscores only, e.g., `my_harvard`) |
| **Name** | A human-readable display name (e.g., "My Harvard Style") |
| **Citation Mode** | Choose **Numbered** or **Author-Year** |
| **In-Text Citation Rules** | Instructions the writing agent follows when placing citations in the text |
| **Bibliography Rules** | Instructions the writing agent follows when formatting the reference list |

4. Click **Create Profile**

!!! warning
    The Profile ID cannot be changed after creation. Choose a clear, descriptive slug.

### Writing Effective Rules

The in-text and bibliography rules are injected directly into the writing agent's system prompt. Write them as clear, imperative instructions. For example:

**In-Text Rules**
```
Use the format (Author, Year) for all in-text citations.
For direct quotes, include the page number: (Author, Year, p. XX).
When citing multiple sources, separate with semicolons: (Author1, Year; Author2, Year).
```

**Bibliography Rules**
```
Format the reference list alphabetically by author surname.
Books: Author, A. A. (Year). Title. Publisher.
Journal articles: Author, A. A. (Year). Title. Journal, Volume(Issue), pages.
Use hanging indent for each entry.
```

!!! tip
    Review the built-in profiles for examples of well-structured rules. The KMU APA 6 and APA 7 profiles demonstrate comprehensive rule sets.

## Editing and Deleting Custom Profiles

- To **edit** a custom profile, click the **pencil icon** next to it, modify the fields, and click **Save Changes**.
- To **delete** a custom profile, click the **trash icon** next to it. If the deleted profile was your default, the system reverts to the Numbered Citations fallback.

!!! warning
    Built-in profiles cannot be edited or deleted.

## Profile Resolution Order

When AXIOM generates a report, it determines which citation profile to use by checking the following in order:

1. **Mission-level override** -- the profile selected in Mission Settings for that specific mission
2. **User default** -- the profile starred in Settings > Citations
3. **System fallback** -- Numbered Citations

The first match in this chain is used.

## Common Issues

- **Citations appear as raw doc_id hashes**: Verify the active profile uses Numbered mode and that post-processing completed successfully. Check the mission logs for errors.
- **Author-year citations missing page numbers**: The writing agent can only include page numbers if the source metadata contains them. Upload documents with complete metadata for best results.
- **Custom profile not appearing**: Ensure you clicked **Create Profile** and that the Profile ID contains only lowercase letters, numbers, and underscores.

## Next Steps

- [Research Configuration](research-config.md) - Configure research mission parameters
- [Writing Mode Overview](../writing/overview.md) - Learn how citation profiles integrate with writing
- [AI Configuration](ai-config.md) - Set up language models used by the writing agent
- [Research Overview](../research/overview.md) - Understand the full research workflow
