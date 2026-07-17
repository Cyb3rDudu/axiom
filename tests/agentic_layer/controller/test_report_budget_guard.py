# Prime axiom_backend's import graph (matches the other controller tests).
import api as _api_primer  # noqa: F401  # isort: skip

import unittest

from ai_researcher.agentic_layer.schemas.planning import SimplifiedPlan, ReportSection
from ai_researcher.agentic_layer.controller.report_generator import ReportGenerator


# ---------------------------------------------------------------------------
# Minimal fakes
# ---------------------------------------------------------------------------

class _FakeContextManager:
    """Captures metadata updates + stored final report for assertions."""

    def __init__(self, mission_context):
        self._mc = mission_context
        self.metadata_updates = []          # list of dicts passed to update_mission_metadata
        self.stored_reports = []            # list of final report strings
        self.status_updates = []            # list of status strings

    def get_mission_context(self, mission_id):
        return self._mc

    async def log_execution_step(self, *args, **kwargs):
        return None

    async def update_mission_metadata(self, mission_id, metadata):
        # Merge into the live metadata so a later read (e.g. the second run)
        # sees the cleared flags, and record the call for assertions.
        self.metadata_updates.append(metadata)
        if self._mc is not None:
            self._mc.metadata.update(metadata)

    def get_notes(self, mission_id):
        return []  # no notes -> "no citations found/needed" completion path

    async def store_final_report(self, mission_id, text):
        self.stored_reports.append(text)

    async def update_mission_status(self, mission_id, status):
        self.status_updates.append(status)


class _FakeController:
    def __init__(self, mission_context):
        self.context_manager = _FakeContextManager(mission_context)


class _MissionContext:
    """A tiny stand-in with the attributes process_citations actually reads."""
    def __init__(self, plan, report_content, metadata):
        self.plan = plan
        self.report_content = report_content
        self.metadata = metadata
        self.reference_id_map = {}


def _build_controller(word_budget_total_max, sections):
    """sections: list of (section_id, title, content, target_words_max)."""
    outline = []
    report_content = {}
    for sid, title, content, tmax in sections:
        outline.append(ReportSection(
            section_id=sid, title=title, description="x",
            research_strategy="research_based", target_words_max=tmax,
        ))
        report_content[sid] = content
    plan = SimplifiedPlan(mission_goal="goal", report_outline=outline)
    metadata = {"word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": word_budget_total_max}}}
    mc = _MissionContext(plan=plan, report_content=report_content, metadata=metadata)
    return _FakeController(mc)


def _para(n_words):
    """A paragraph of ~n_words words with sentence punctuation so the trim
    function can cut at a sentence boundary (realistic German text)."""
    words = []
    for i in range(n_words):
        words.append("Satzende." if i % 8 == 7 else "Wort")
    return " ".join(words)


class TestReportBudgetGuard(unittest.IsolatedAsyncioTestCase):
    """Direct regression tests for ReportGenerator.process_citations() budget path
    (review round 3, finding 2). Two scenarios: over budget and within budget.
    """

    async def test_trim_pass_cuts_sections_to_hard_max(self):
        # total_max = 100. Two sections, each target_words_max=50, but their
        # STORED content is 80 words each (160 total). The HARD trim pass must
        # cut each to its target_words_max (<=50). After trimming, content
        # (100) is within budget -> no banner, mission completed clean.
        controller = _build_controller(
            word_budget_total_max=100,
            sections=[
                ("s1", "Einleitung", _para(80), 50),
                ("s2", "Analyse", _para(80), 50),
            ],
        )
        rg = ReportGenerator(controller)

        ok = await rg.process_citations("m1")
        self.assertTrue(ok)
        cm = controller.context_manager

        # (a) stored sections trimmed to the EXACT hard max (<=50) each.
        self.assertLessEqual(
            len(cm._mc.report_content["s1"].split()), 50,
            "s1 not trimmed to target_words_max",
        )
        self.assertLessEqual(
            len(cm._mc.report_content["s2"].split()), 50,
            "s2 not trimmed to target_words_max",
        )
        # (b) final report captured and completed.
        self.assertEqual(len(cm.stored_reports), 1)
        self.assertEqual(cm.status_updates[-1], "completed")
        # (c) content is within budget after trim -> no banner.
        self.assertNotIn("Hinweis zur Wortanzahl", cm.stored_reports[0])

    async def test_genuine_over_budget_after_trim_counts_content_only(self):
        # total_max = 100 but section budgets sum to 200 (two sections each
        # target_words_max=100), content 100 each (200 total). Trimming to
        # target_words_max still leaves 200 content > 100 total -> genuine
        # over-budget: banner + warning, and content_words must count CONTENT
        # only (never headings).
        controller = _build_controller(
            word_budget_total_max=100,
            sections=[
                ("s1", "Einleitung einer sehr langen Ueberschrift", _para(100), 100),
                ("s2", "Analyse", _para(100), 100),
            ],
        )
        rg = ReportGenerator(controller)
        ok = await rg.process_citations("m1")
        self.assertTrue(ok)
        cm = controller.context_manager

        exceeded = None
        for upd in cm.metadata_updates:
            if upd.get("word_budget_exceeded") is not None:
                exceeded = upd["word_budget_exceeded"]
        self.assertIsNotNone(exceeded, "content over total_max must flag over-budget")
        # content_words counts the SECTION CONTENT only (200), never headings.
        self.assertEqual(exceeded["content_words"], 200)
        self.assertEqual(exceeded["over_by"], 100)
        self.assertEqual(exceeded["budget_max"], 100)
        # heading_words tracked separately and > 0 (two headings present).
        self.assertGreater(exceeded["heading_words"], 0)
        # final_file_words >= content + headings (banner adds more).
        self.assertGreaterEqual(
            exceeded["final_file_words"],
            exceeded["content_words"] + exceeded["heading_words"],
        )
        # Banner present and reports the CONTENT count.
        final_report = cm.stored_reports[0]
        self.assertIn("Hinweis zur Wortanzahl", final_report)
        self.assertIn("200", final_report)
        # completed_with_word_budget_warning set.
        self.assertTrue(any(
            upd.get("completed_with_word_budget_warning") is True
            for upd in cm.metadata_updates
        ))

    async def test_headings_do_not_trigger_false_overrun_and_clears_stale(self):
        # Review finding 1: total_max = 100. Two sections, each content exactly
        # 50 words (100 total content, within budget). The ASSEMBLED draft adds
        # two headings, pushing the FILE past 100, but content is within budget
        # -> NO warning, NO banner. Also proves stale warning flags are cleared.
        controller = _build_controller(
            word_budget_total_max=100,
            sections=[
                ("s1", "Einleitung", _para(50), 60),
                ("s2", "Analyse der externen Entwicklung", _para(50), 60),
            ],
        )
        # Pre-seed stale over-budget warning to prove the OK branch clears it.
        controller.context_manager._mc.metadata["word_budget_exceeded"] = {"old": True}
        controller.context_manager._mc.metadata["completed_with_word_budget_warning"] = True

        rg = ReportGenerator(controller)
        ok = await rg.process_citations("m1")
        self.assertTrue(ok)
        cm = controller.context_manager

        # The assembled final report (with headings) exceeds 100 words...
        final_report = cm.stored_reports[0]
        self.assertGreater(
            len(final_report.split()), 100,
            "sanity: headings push file over total_max",
        )
        # ...but no warning/banner because CONTENT (100) is within budget.
        self.assertNotIn("Hinweis zur Wortanzahl", final_report)
        # The OK branch must CLEAR the stale warning metadata.
        self.assertTrue(any(
            "word_budget_exceeded" in upd and upd["word_budget_exceeded"] is None
            for upd in cm.metadata_updates
        ), "stale word_budget_exceeded must be cleared on a within-budget run")
        self.assertTrue(any(
            "completed_with_word_budget_warning" in upd
            and upd["completed_with_word_budget_warning"] is None
            for upd in cm.metadata_updates
        ), "stale completed_with_word_budget_warning must be cleared")
        # And the live metadata no longer carries the stale flags.
        self.assertFalse(cm._mc.metadata.get("word_budget_exceeded"))
        self.assertFalse(cm._mc.metadata.get("completed_with_word_budget_warning"))


if __name__ == "__main__":
    unittest.main()


# ---------------------------------------------------------------------------
# _persist_final_word_metrics: final_file_words must equal the stored string
# (review round 4). The report gains a title, portfolio and references AFTER
# the early budget decision, so metrics must be computed from the EXACT final
# string passed to store_final_report().
# ---------------------------------------------------------------------------

class _MetricsContextManager:
    """Minimal fake: returns a mission context with configurable metadata and
    captures the metadata updates from _persist_final_word_metrics."""
    def __init__(self, metadata):
        self._mc = _MissionContext(plan=None, report_content={}, metadata=dict(metadata))

    def get_mission_context(self, mission_id):
        return self._mc

    async def update_mission_metadata(self, mission_id, metadata):
        self._mc.metadata.update(metadata)
        self._last = metadata


class _MetricsController:
    def __init__(self, metadata):
        self.context_manager = _MetricsContextManager(metadata)


class TestPersistFinalWordMetrics(unittest.IsolatedAsyncioTestCase):
    async def test_final_file_words_equals_stored_with_title_portfolio_refs(self):
        """A report containing a title, a literature portfolio and a references
        section: final_file_words must equal len(final_string.split()), and the
        content/heading/banner/reference breakdown must sum to it."""
        content_body = _para(40)                       # 40 content words
        title_block = "# NexMach als Unternehmung\n\n"  # a title heading
        portfolio = "\n\n## Literaturportfolio\n\nEntry A. Entry B.\n"
        references = "\n\n## References\n\n1. Smith (2020). 2. Jones (2021)."
        final_string = title_block + content_body + portfolio + references

        controller = _MetricsController({
            "word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}
        })
        rg = ReportGenerator(controller)

        content_words = 40
        banner_words = 0
        reference_words = len(portfolio.split()) + len(references.split()) + len(title_block.split())

        await rg._persist_final_word_metrics(
            "m1", final_string, content_words, banner_words, reference_words,
        )

        meta = controller.context_manager._mc.metadata
        exceeded = meta["word_budget_exceeded"]
        # content (40) is within max (100) -> flags cleared, not set.
        self.assertIsNone(exceeded)
        self.assertFalse(meta.get("completed_with_word_budget_warning"))

    async def test_over_budget_final_file_words_matches_stored(self):
        """Over budget AND the stored string has title+portfolio+refs:
        final_file_words == len(final_string.split()) exactly, even though
        those add words the early measurement would have missed."""
        content_body = _para(120)                      # 120 content -> over max 100
        title_block = "# Titel\n\n"
        portfolio = "\n\n## Literaturportfolio\n\nA B C D E F G H I J.\n"
        references = "\n\n## References\n\n1. A. 2. B. 3. C."
        final_string = title_block + content_body + portfolio + references

        controller = _MetricsController({
            "word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}
        })
        rg = ReportGenerator(controller)

        reference_words = (
            len(title_block.split()) + len(portfolio.split()) + len(references.split())
        )
        await rg._persist_final_word_metrics(
            "m1", final_string, content_words=120, banner_words=0,
            reference_words=reference_words,
        )

        meta = controller.context_manager._mc.metadata
        exceeded = meta["word_budget_exceeded"]
        self.assertIsNotNone(exceeded)
        # THE core assertion: final_file_words is EXACTLY the stored string length.
        self.assertEqual(exceeded["final_file_words"], len(final_string.split()))
        self.assertEqual(exceeded["content_words"], 120)
        self.assertEqual(exceeded["budget_max"], 100)
        self.assertEqual(exceeded["over_by"], 20)
        self.assertEqual(exceeded["reference_words"], reference_words)
        self.assertEqual(exceeded["banner_words"], 0)
        # breakdown sums to final_file_words (heading_words is the residual).
        self.assertEqual(
            exceeded["content_words"] + exceeded["heading_words"]
            + exceeded["banner_words"] + exceeded["reference_words"],
            exceeded["final_file_words"],
        )
        self.assertTrue(meta.get("completed_with_word_budget_warning"))

    async def test_banner_words_tracked_separately_from_headings(self):
        """When a banner is present it is counted as banner_words, NOT folded
        into heading_words (review round 4 complaint)."""
        content_body = _para(120)
        banner = "> Hinweis zur Wortanzahl. Bitte kuerzen.\n\n"
        final_string = banner + content_body

        controller = _MetricsController({
            "word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}
        })
        rg = ReportGenerator(controller)
        banner_words = len(banner.split())
        await rg._persist_final_word_metrics(
            "m1", final_string, content_words=120, banner_words=banner_words,
            reference_words=0,
        )
        exceeded = controller.context_manager._mc.metadata["word_budget_exceeded"]
        self.assertEqual(exceeded["banner_words"], banner_words)
        self.assertGreater(banner_words, 0)
        self.assertEqual(exceeded["final_file_words"], len(final_string.split()))
        # breakdown still sums exactly.
        self.assertEqual(
            exceeded["content_words"] + exceeded["heading_words"]
            + exceeded["banner_words"] + exceeded["reference_words"],
            exceeded["final_file_words"],
        )


class TestPersistFailureMarksMission(unittest.IsolatedAsyncioTestCase):
    """Review round 5, issue 4: a metric-persistence failure must NOT be
    swallowed silently. The mission is marked with word_metrics_persistence_failed
    so a missing/stale budget state can never masquerade as a clean OK run."""

    async def test_db_failure_does_not_propagate(self):
        """A DB failure during the real budget write must not crash report
        completion."""

        class _ExplodingCM(_MetricsContextManager):
            async def update_mission_metadata(self, mission_id, metadata):
                raise RuntimeError("DB connection lost")

        controller = _MetricsController({
            "word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}
        })
        controller.context_manager = _ExplodingCM(
            {"word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}}
        )
        rg = ReportGenerator(controller)

        # Must NOT raise — the report should still complete.
        await rg._persist_final_word_metrics(
            "m1", "some final report content", content_words=40,
            banner_words=0, reference_words=0,
        )

    async def test_persistence_failed_flag_recorded(self):
        """When the budget write fails but the failure-flag write succeeds, the
        flag is recorded on the mission metadata."""
        calls = {"n": 0}

        class _FailOnceThenOK:
            def __init__(self):
                self._mc = _MissionContext(
                    plan=None, report_content={},
                    metadata={"word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}}},
                )

            def get_mission_context(self, mission_id):
                return self._mc

            async def update_mission_metadata(self, mission_id, metadata):
                calls["n"] += 1
                # First call (the real budget write) fails; the second call
                # (the failure-flag write) succeeds.
                if calls["n"] == 1:
                    raise RuntimeError("DB connection lost")
                self._mc.metadata.update(metadata)

        controller = _MetricsController({})
        controller.context_manager = _FailOnceThenOK()
        rg = ReportGenerator(controller)

        await rg._persist_final_word_metrics(
            "m1", "final report content", content_words=120,
            banner_words=0, reference_words=0,
        )

        meta = controller.context_manager._mc.metadata
        self.assertIn("word_metrics_persistence_failed", meta)
        self.assertIn("DB connection lost", meta["word_metrics_persistence_failed"]["error"])

    async def test_persistence_failed_flag_cleared_on_successful_run(self):
        """Review round 6, issue 2: a successful metrics-persist run must CLEAR
        a stale word_metrics_persistence_failed flag left by a prior transient
        DB failure, so a temporary error does not linger as a permanent failure
        status."""
        # Mission metadata carries a stale failure flag from a previous run.
        controller = _MetricsController({
            "word_budget": {"total_word_budget": {"min": 1, "target": 1, "max": 100}},
            "word_metrics_persistence_failed": {"error": "DB connection lost"},
        })
        rg = ReportGenerator(controller)

        # This run is within budget (40 <= 100) and the write succeeds.
        await rg._persist_final_word_metrics(
            "m1", "final report content", content_words=40,
            banner_words=0, reference_words=0,
        )

        meta = controller.context_manager._mc.metadata
        # The success path must have cleared the stale failure flag (set to None).
        self.assertIsNone(
            meta.get("word_metrics_persistence_failed"),
            "successful persist must clear the stale failure flag",
        )
        # And the budget-exceeded flags stay cleared too.
        self.assertIsNone(meta.get("word_budget_exceeded"))
        self.assertFalse(meta.get("completed_with_word_budget_warning"))
