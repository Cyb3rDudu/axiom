# Literature Portfolio Agent — Design Doc

**Status:** Implementation in progress
**Scope:** Post-writing literature reflection deliverable for research missions
**Motivation:** KMU Akademie (Partnership Middlesex University London) requires an obligatory tabular *Literaturportfolio* for every written assignment from 15.04.2026 onward. The portfolio documents the research process, source relevance per section, and quality assessment per source. Axiom should produce this automatically as a first-class deliverable for missions that resemble academic coursework — and selectively skip it when it is not wanted.

## 1. Goals

1. Generate a KMU-compliant literature portfolio (Quellenangabe | Recherchetool | Relevanz | Qualität) for every research mission **by default**.
2. Allow explicit opt-out via user keyword ("ohne Literaturportfolio", "kein Portfolio", "no portfolio", "skip portfolio") or explicit API flag.
3. Produce structured output (JSON) *and* rendered markdown table — markdown is appended to the final report; JSON is persisted in `mission_settings.deliverables.literature_portfolio_output` for UI rendering and later export.
4. Evaluate each source along signal-backed dimensions (publication type, peer-review, publisher tier, recency, author credentials) — not free-text.
5. Run a compliance check against KMU's numeric thresholds (10–20 sources, ≥50 % wissenschaftlich/facheinschlägig, blacklist) and emit a traffic-light summary.
6. Bilingual (DE default for missions with `language_code='de'`, EN fallback).
7. Low incremental cost: one additional Sonnet-4.6 call per mission, reusing existing metadata enrichment.

## 2. Non-goals (deferred)

- Live Scimago API integration (MVP uses a hard-coded publisher tier list + CrossRef signals).
- Dedicated `source_quality_assessments` cache table (MVP computes inline; add caching if latency becomes an issue).
- Word/DOCX export with KMU template (axiom already exports markdown; user converts for now).
- KI-Nutzungsverzeichnis auto-generation (separate concern; follow-up task).
- Changes to `Note` schema. We piggyback on the citation graph emitted by `report_generator.process_citations` — no schema diff needed for MVP.
- UI checkbox / frontend changes (keyword-based opt-out is enough until the backend is stable).

## 3. Opt-out semantics (default ON)

| Trigger | Source | Effect |
|---|---|---|
| User request contains `"ohne literaturportfolio"` \| `"kein portfolio"` \| `"no portfolio"` \| `"skip portfolio"` (case-insensitive, word-boundary) | raw user message at mission creation | sets `mission_settings.deliverables.literature_portfolio = False` |
| API payload: `literature_portfolio: false` | JSON field on `POST /missions` | overrides default |
| Default | anywhere else | `True` |

Detection lives in `api/missions.py::create_mission` right after `user_request` is read. The check is a ~10-line pure function in `ai_researcher/agentic_layer/controller/utils/portfolio_optout.py` so it's unit-testable without the FastAPI layer.

## 4. Data flow

```
┌────────────────────────┐
│ Research + Writing      │   (existing — no change)
└──────────┬─────────────┘
           ▼
┌────────────────────────┐
│ report_generator        │   stores final_report, reference_id_map
│ .process_citations      │
└──────────┬─────────────┘
           ▼ NEW HOOK (before update_mission_status → "completed")
┌────────────────────────┐
│ LiteraturePortfolio     │   input : full_draft, citation graph,
│ Manager.build_portfolio │           all_notes, mission_context
│ (new helper class)      │   does  : source aggregation, signal
│                         │           computation, agent call
│                         │   output: PortfolioOutput (JSON)
└──────────┬─────────────┘
           ▼
┌────────────────────────┐
│ Final report updated    │   markdown table appended before
│ with portfolio section  │   "## References"
└──────────┬─────────────┘
           ▼
┌────────────────────────┐
│ update_mission_status   │   → "completed"
│ (unchanged)             │
└────────────────────────┘
```

Key: the hook fires **after** `process_citations` has identified the `used_doc_ids` set, so the portfolio only lists sources that are *actually cited* in the final draft. That matches KMU's definition ("verwendete Quellen") and avoids listing retrieved-but-unused sources.

## 5. Integration points (precise file locations)

| # | File | Line (approx.) | Change |
|---|---|---|---|
| 1 | `api/missions.py` | ~515 (`comprehensive_settings`) | Inject `deliverables` sub-dict with default-on + opt-out detection |
| 2 | `ai_researcher/agentic_layer/controller/utils/portfolio_optout.py` | new | Pure function `detect_portfolio_optout(user_request) -> bool` |
| 3 | `ai_researcher/agentic_layer/controller/report_generator.py` | 508–520, 818–820 | Before each `update_mission_status(... "completed")`: invoke `LiteraturePortfolioManager.run_if_enabled(mission_id, full_draft, used_doc_ids, doc_metadata_source, log_queue, update_callback)` and, on success, append the rendered markdown to `final_report_string`. |
| 4 | `ai_researcher/agentic_layer/controller/literature_portfolio_manager.py` | new | Orchestrator: reads mission flag, builds source records with quality signals, calls agent, renders markdown, stores structured output. |
| 5 | `ai_researcher/agentic_layer/agents/literature_portfolio_agent.py` | new | Agent class, DE default prompt, EN fallback, structured output schema. |
| 6 | `ai_researcher/agentic_layer/schemas/portfolio.py` | new | `PortfolioEntry`, `PortfolioOutput`, `ComplianceReport` Pydantic schemas. |
| 7 | `ai_researcher/agentic_layer/services/source_quality.py` | new | `compute_quality_signals(note: Note) -> QualitySignals` — publisher-tier lookup, peer-review heuristic, recency. |
| 8 | `ai_researcher/agentic_layer/services/publisher_tiers.py` | new | Curated tier list (Tier A scientific publishers, Tier B reputable institutes, Tier D blacklist). |
| 9 | `database/migrations/add_literature_portfolio.sql` | new | Add `literature_portfolio_output JSONB` column to `missions` for durable storage of the portfolio output. (Kept separate from `mission_context` blob for easier analytics.) |
| 10 | `tests/test_portfolio_optout.py` | new | Opt-out keyword detection |
| 11 | `tests/test_source_quality.py` | new | Publisher tier + signals |
| 12 | `tests/test_literature_portfolio_agent.py` | new | Snapshot test on fixed fixture |

## 6. Schema: `PortfolioOutput`

```python
class QualitySignals(BaseModel):
    publication_type: Literal[
        "peer_reviewed_journal", "monograph_scientific_publisher",
        "edited_book", "conference_proceedings", "working_paper",
        "preprint", "industry_report", "whitepaper", "news_article",
        "blog", "data_portal", "standard", "legal_document",
        "web_page", "unknown"
    ]
    peer_reviewed: Optional[bool] = None
    publisher_tier: Literal["A", "B", "C", "D", "blacklist", "unknown"]
    journal_name: Optional[str] = None
    publisher: Optional[str] = None
    has_doi: bool = False
    recency_years: Optional[int] = None
    author_credentials_note: Optional[str] = None
    bias_flags: list[str] = Field(default_factory=list)

class PortfolioEntry(BaseModel):
    source_id: str                    # doc_id or hashed URL
    apa_citation: str                 # rendered APA 7 string
    discovery_tool: str               # "Local Library", "Google Scholar",
                                      # "CrossRef", "Web Search", "OpenAlex",
                                      # "arXiv", "ProQuest", ...
    relevance_bullets: list[str]      # 1-3 bullet points, KMU-style
    quality_bullets: list[str]        # 1-3 bullet points, KMU-style
    quality_signals: QualitySignals
    sections_used_in: list[str]       # section IDs where cited
    contribution_type: Literal[
        "theory", "empirical", "background",
        "counter_position", "definition", "data_source", "practice"
    ]
    scientific_tier: Literal["A", "B", "C", "D"]  # A+B = wissenschaftlich

class ComplianceReport(BaseModel):
    source_count: int
    source_count_ok: bool                     # 10 ≤ n ≤ 20
    scientific_share: float                   # 0.0 – 1.0
    scientific_share_ok: bool                 # ≥ 0.5
    blacklist_hits: list[str]                 # Wikipedia, tabloids, ...
    recency_warnings: list[str]               # sources older than 10 years
    traffic_light: Literal["green", "yellow", "red"]
    advice: list[str]                         # concrete fix suggestions

class PortfolioOutput(BaseModel):
    mission_id: str
    language_code: Literal["de", "en"]
    generated_at: datetime
    entries: list[PortfolioEntry]
    compliance: ComplianceReport
    markdown_table: str                       # rendered, ready to append
```

## 7. Publisher tier list (v0)

Hard-coded in `services/publisher_tiers.py`:

- **Tier A** (top-tier scientific publishers, peer-reviewed): Springer / Springer Nature, Wiley, Elsevier, Sage, Routledge (Taylor & Francis), Oxford University Press, Cambridge University Press, MIT Press, IEEE, ACM, Emerald, Palgrave Macmillan, De Gruyter, Nomos, JSTOR-hosted journals.
- **Tier B** (reputable research institutions + standards bodies): IMF, World Bank, OECD, UNCTAD, WTO, BIS, ECB, European Commission JRC, ifo, DIW, IW Köln, WIFO, IHS, ZEW, Bruegel, PIIE, CSIS, MERICS, Rhodium Group, Brookings, RAND, NBER, CEPR, SSRN, RePEc-listed working-paper series, ISO, DIN.
- **Tier C** (scientific but unverified): preprints (arXiv, bioRxiv, SSRN preprints without peer-review signal), unknown academic publishers.
- **Tier D** (practitioner / grey literature): corporate whitepapers, consulting reports (McKinsey, BCG, Deloitte, PwC, KPMG, EY), industry associations, trade press.
- **Blacklist** (disallowed as primary scientific source per KMU "Dos and Don'ts"): Wikipedia, Gabler Wirtschaftslexikon, Investopedia, daily newspapers as theoretical basis, Medium posts, random blogs, SEO content farms.

Matched against the `source_metadata.url` / `publisher` / `doc_id` via substring contains on normalised domain/publisher name. Extensible — loading from a YAML later is trivial.

## 8. Scientific tier assignment (policy)

```
publisher_tier == "A" AND peer_reviewed != False            → "A"
publisher_tier == "A" AND peer_reviewed is None             → "A"
publisher_tier == "B"                                       → "B"
publisher_tier == "C"                                       → "C"
publisher_tier == "D"                                       → "D"
publisher_tier == "blacklist"                               → "D" + blacklist_hit
publication_type in {"peer_reviewed_journal"}               → force "A"
publication_type in {"preprint","working_paper"}            → max(current, "C")
no signals resolvable                                       → "C" (conservative)
```

"Scientific share" for compliance = `count(tier in {A,B}) / total_count`.

## 9. Agent prompt (sketch)

System prompt (DE, ~180 lines) structure:
- Role: reflektierender Quellen-Evaluator an der KMU Akademie
- Inputs (in user prompt): mission goal, list of source records with pre-computed quality signals and contextual snippets (how the source was used across sections), language.
- Output: JSON matching `PortfolioOutput` schema. Strict mode via Pydantic.
- Style rules: Relevanz/Qualität bullets mirror the KMU example exactly — short, imperative, source-specific. No filler. Draw on pre-computed signals rather than inventing quality claims.
- CRITICAL rules:
  - Never fabricate author affiliations or journal rankings.
  - Every bullet must reference a section or a signal.
  - If `publisher_tier == "blacklist"`: surface the warning in `quality_bullets` and in `compliance.blacklist_hits`.
  - Bullets in the same language as `language_code`.

EN version is a structural mirror.

Prompts seeded into `prompt_templates` table via a seeder script so they follow the existing bilingual loading pattern.

## 10. Markdown rendering

The markdown table matches the KMU PDF example 1:1:

```markdown
## Literaturportfolio

_Automatisch erstellt am 2026-04-20. Umfasst alle im Bericht tatsächlich zitierten Quellen._

| Quellenangabe (lt. Literaturverzeichnis) | Recherchetool | Relevanz | Qualität |
|---|---|---|---|
| Contreras, F., Baykal, E., & Abid, G. (2020)… | Google Scholar | • Grundlegende Theorie …<br>• Wichtige Faktoren … | • Peer-reviewte Fachzeitschrift<br>• Hohe Aktualität (2020)<br>• methodisch transparent |
| … | | | |

**Compliance-Ampel: 🟢 grün**
- 14 Quellen (Zielkorridor 10–20)
- 57 % wissenschaftlich/facheinschlägig (Ziel ≥ 50 %)
- Keine Blacklist-Treffer
```

Appended to the final report **between** the body and the `## References` section, so the existing references block stays intact.

## 11. Failure modes & fallbacks

| Scenario | Behaviour |
|---|---|
| `deliverables.literature_portfolio == False` | Skip entirely, log one info line. |
| Zero cited sources | Skip portfolio generation; add a short note instead: "No cited sources found — Literaturportfolio nicht erstellt." |
| Quality signal lookup errors for a source | Fill with `publisher_tier="unknown"`, flag source, continue. |
| Agent call fails after retries | Log error, store partial `PortfolioOutput` with empty entries + `traffic_light="red"` and advice message "portfolio generation failed — manual review recommended". Do **not** fail the overall mission. |
| Non-DE, non-EN language | Use EN prompt; emit portfolio in EN; log warning. |

## 12. Testing

- `tests/test_portfolio_optout.py` — opt-out detection: positive and negative cases in DE and EN, word-boundary correctness.
- `tests/test_source_quality.py` — publisher tier matching, peer-review detection, recency, blacklist.
- `tests/test_literature_portfolio_agent.py` — fixture mission with 4 cited sources, mock LLM response, assert rendered markdown structure + compliance counts.
- Integration: extend an existing end-to-end mission test (if one exists; otherwise document manual test plan).

Target coverage for new Python modules: ≥90 % (axiom-backend does not have the `100 %` Go-package rule — that's only `axiom_backend_ng`).

## 13. Rollout plan

1. Land schema + opt-out detection + stub agent behind `deliverables.literature_portfolio` flag.
2. Seed prompts in DE + EN into `prompt_templates`.
3. Enable default-on in staging. Run the VWL-Hausarbeit mission end-to-end.
4. Review generated portfolio manually against the KMU Anleitung example — tune prompt + publisher tiers.
5. Promote to production. Monitor: portfolio generation latency, traffic-light distribution, any silent failures (error counter in logs).

## 14. Open questions

- Should the portfolio be stored as a separate field in `missions` (first-class, analytics-friendly) or inline in `mission_settings` (zero-schema-change)? **Decision for MVP**: new JSONB column on `missions`. Small migration, cleaner queries.
- Where should the `discovery_tool` string come from? Currently axiom tracks `source_type` (`document` / `web` / `internal`) but not *which* tool retrieved the source. **MVP mapping**: `document` → "Local Library (RAG)", `web` → `search_provider` from `mission_settings.comprehensive_settings`, `internal` → "Agent Synthesis". A richer per-note `retrieved_via` field is a follow-up.
- Compliance threshold tuning (10–20) may drift. **Decision**: keep as constants in `services/publisher_tiers.py` for easy editing; resist promoting to DB config until needed.
