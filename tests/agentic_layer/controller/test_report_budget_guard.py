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
