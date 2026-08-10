# Prime axiom_backend's import graph (matches the other controller tests).
import api as _api_primer  # noqa: F401  # isort: skip

import unittest

from ai_researcher.agentic_layer.async_context_manager import (
    MissionContext as AsyncMissionContext,
)
from ai_researcher.agentic_layer.context_manager import (
    MissionContext as SyncMissionContext,
)


class TestReportContentNoneCoercion(unittest.TestCase):
    """A failed section write can store None in report_content (e.g. mission
    7acd5677 had report_content.einleitung = None). Without coercion this makes
    the ENTIRE MissionContext fail Pydantic validation on load, corrupting the
    whole mission. The validator coerces None -> '' so one failed write cannot
    make a mission unloadable."""

    def _mk(self, cls):
        return cls(user_request="x", report_content={
            "einleitung": None,
            "fazit": "Some real text.",
            "hauptteil": None,
        })

    def test_async_coerces_none_to_empty(self):
        mc = self._mk(AsyncMissionContext)
        self.assertEqual(mc.report_content["einleitung"], "")
        self.assertEqual(mc.report_content["hauptteil"], "")
        self.assertEqual(mc.report_content["fazit"], "Some real text.")

    def test_sync_coerces_none_to_empty(self):
        mc = self._mk(SyncMissionContext)
        self.assertEqual(mc.report_content["einleitung"], "")
        self.assertEqual(mc.report_content["hauptteil"], "")
        self.assertEqual(mc.report_content["fazit"], "Some real text.")

    def test_empty_dict_ok(self):
        mc = AsyncMissionContext(user_request="x")
        self.assertEqual(mc.report_content, {})

    def test_non_dict_passthrough_safe(self):
        # A non-dict report_content is malformed data; Pydantic's Dict[str,str]
        # type validation (after our before-validator) correctly rejects it.
        # This is the desired behavior — we only coerce None VALUES, we do not
        # accept garbage shapes.
        with self.assertRaises(Exception):
            AsyncMissionContext.model_validate(
                {"user_request": "x", "report_content": "not a dict"}
            )


if __name__ == "__main__":
    unittest.main()
