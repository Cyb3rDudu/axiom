# Doc-Agent Profile (concept)

> **Status: concept phase — design, not yet piloted.** The doc-agent is a
> planned agent profile that turns code changes (and the surrounding review
> context) into documentation updates automatically as a pull request. Per the
> epic, automation only runs against a *finished* documentation foundation, and
> every change still passes through a human review gate. This page is the
> design: what is automatable, how the profile behaves, and where its limits
> are.

The goal is not full autonomy. The goal is a **multiplier**: a profile that can
carry the mechanical, high-volume, low-judgment doc updates that accumulate
with every code change, so humans keep the conceptual work and the final gate.

## Why it is the last layer

Automation built on a half-finished structure produces half-finished output at
high speed. The doc-agent is deliberately the last documentation layer (after
D1–D6) so that, when it runs, it works against a stable site, a settled
information architecture, and a settled naming rule set. Under those
conditions it becomes fast and predictable instead of amplifying chaos.

## The automatisability matrix

The core design decision is *which documentation kinds are safe to automate*
and *which must stay human*. The judgment questions are:

- Does the update **derive mechanically** from a diff, an env table, a schema,
  or a contract? → strong candidate for automation.
- Does the update require **interpreting intent** (why a choice was made,
  whether a pattern still fits, whether a troubleshooting symptom is still
  real)? → human.

| Documentation kind | Output derives from | Automatable? | Notes |
| --- | --- | --- | --- |
| Configuration reference (env-var tables) | The `AXIOM_*` env set in code + defaults | **Yes** | Mechanical: var name, default, one-line meaning. The doc-agent can diff the code's config surface against the published table. |
| Endpoint list | The route table in `axiom_ng`/the contract | **Yes** | Mechanical: method + path + purpose. Changes on contract/route additions. |
| Changelog fragment per merge | The merge's commit messages + issue context | **Yes (fragment)** | Generate a *draft* fragment, but a human decides what is user-visible. |
| Data-model cheat-sheet on a migration | The new migration's schema | **Yes** | Additive column/table changes map directly; semantic meaning stays human. |
| Data-model **relationships/semantics** (why a column exists, invariants) | Reasoning, not code | **No** | Human. The schema tells you *what*, not *why*. |
| Architecture overview / design decisions | Intent and trade-offs | **No** | Explicitly out of scope (epic non-goal). |
| Concept docs (Welcome, Concept Tour) | Product narrative | **No** | Human; narrative is not derivable. |
| Troubleshooting symptom patterns | Real recurring incidents, not diffs | **No** | Patterns only from lived experience; a diff does not imply a symptom. |
| Benchmark/measurement reports | Dated measurement runs | **No** | Dated by nature; never auto-revised. |

**Boundary rule of thumb:** if the target text can be *recomputed* from the
code (tables, lists, spellouts), automate; if it must be *written* with
judgment (explanations, decisions, narratives), keep it human.

## What the profile does and does not touch

On trigger (below) the doc-agent produces a **draft** doc update that stays in
a pull request. It is a *consumer* of verification, not a replacement: it reads
the change's issue/review context to know *what actually changed and why*, but
it never produces or signs off on verification itself.

Non-goals that are hard boundaries:

- **No full autonomy:** never a direct commit to `main`; the human gate is
  unconditional.
- **No new autonomous sections:** the agent may update existing pages and
  follow the existing nav/style; it must never invent new top-level sections or
  restructure the site on its own.
- **No architectural-doc automation:** concept/architecture/sizing content stays
  human.

## Trigger and scope

The profile activates on **code-bearing commit scopes**, not on every commit.
Proposed trigger:

- Commit scope `feat|fix` that touches code paths whose doc surface changed
  (e.g. the config/env loader, the HTTP router, a DB migration),
- OR an endpoint/config/data-model change referenced in an issue/review.

It does **not** trigger on docs-only commits, chore-only commits, or research
merges that carry no doc-relevant code surface.

## Rules for produced output (so it merges without rework)

1. **Match existing ship** — English, product name "axiom", module paths and
   `AXIOM_*` names as technical facts (the settled naming rule set).
2. **Update, don't restructure** — touch the existing page; never add sections
   or reorganize the nav on its own.
3. **State the change, not the history** — diff-driven update, no "previously
   X, now Y" narration.
4. **Every change carries a rationale** in the PR body: what changed in code,
   which page/table it lands on, and why it is in scope for automation.
5. **Output is a PR**, not a commit — a human (and the doc pipeline's CI as a
   check) is the gate.

## Site-build as the CI check

The generated pull request runs the documentation CI (the same `mkdocs build
--clean --strict` that the site uses, plus the naming and generalizability
gates). A doc-agent PR cannot merge if it introduces a dead link, a naming
violation, or private-infra residue. The gates that were built into the docs
pipeline become the agent's own acceptance tests — that's the payoff of having
the foundation first.

## Interface to the Hivemind workflow

The doc-agent consumes review context as a **secondary source**:

- From the issue/PR context it reads *what changed and why* — the same narrative
  a Hivemind review would capture (e.g. "added the `AXIOM_*` timeout, with a
  near-miss twin").
- It uses that to decide *which* doc surface to update and *how to word* the
  change, but never to claim verification.

The interface is: **trigger → gather code-surface diff + issue/review context →
produce draft PR → human/Hivemind reviews the doc diff specifically → merge.**
Hivemind remains the producer of verification; the doc-agent is its consumer.

## Pilot plan (after the foundation is stable)

A pilot runs only once the working foundation is merged and live (D1–D6 on
`main`), so the agent targets the real site, not a moving target.

- **Pick one real change:** a config/env change or an endpoint/code addition
  that touches a doc surface the matrix marks automatic.
- **Run the profile:** it produces the draft doc PR with rationale.
- **Outcome measure (both are a win):**
  1. the PR merges **without manual rework**, or
  2. the agent's boundaries are cleanly documented (what it got right, what a
    human had to fix) — equally valuable for scoping.

The pilot deliberately starts small and mechanical (a single env table or
endpoint list update) so the "recompute-from-code" path is proven before any
judgment is asked of it.

## Current status

- [x] Automatisability matrix (this page).
- [x] Profile spec: trigger, rules, PR-gate, CI-check, Hivemind interface.
- [ ] Pilot on a real change — **deferred until the D1–D6 foundation is merged
  and live**, per the epic's ordering (D7 is the last layer).

Continue: [Developer Guide](architecture.md) ·
[Testing](testing.md)
