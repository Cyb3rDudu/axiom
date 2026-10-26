"""Relationship checkpoints must carry entities appended by extraction."""

import json
import sys
import types

from axiom_compute_worker import runner
from axiom_compute_worker.checkpoints import PhaseCheckpoints
from axiom_compute_worker.config import Settings, settings

from test_checkpoint_resume import _request, _workdir


def test_resumed_relationships_restore_appended_entities(tmp_path, monkeypatch):
    src = tmp_path / "book.pdf"
    src.write_bytes(b"%PDF-fixture")  # conversion is checkpointed
    req = _request(src, "relationship-resume")
    req["processing"].update(extract_entities=True, extract_relationships=True)
    work_root = tmp_path / "work-root"
    old = settings.get()
    settings.set(Settings(work_root=work_root,
                          allowed_source_roots=(str(tmp_path),),
                          compute_backend="real", warmup=False))
    try:
        page_trust = types.ModuleType("axiom_compute_worker.compute_core.page_trust")
        page_trust.PHYSICAL_ONLY = "physical_only"
        monkeypatch.setitem(sys.modules, page_trust.__name__, page_trust)
        md = tmp_path / "seed.md"
        md.write_text("Alpha works at Beta Corp", encoding="utf-8")
        cp = PhaseCheckpoints(req, work_root)
        cp.save("convert", files={"markdown.md": md}, jsons={
            "image_mapping": {}, "page_label_map": {}, "page_source_map": {},
            "page_chapter_map": {}, "marker_pagemap_max": None, "cfi_entries": []})
        chunks = [{"text": "Alpha works at Beta Corp",
                   "metadata": {"start_paragraph_index": 0}}]
        cp.save("chunk", jsons={"chunks": chunks})
        cp.skip("embed")
        first_entity = {"ref": "entity-0001", "text": "Alpha",
                        "canonical_form": "alpha", "type": "CONCEPT",
                        "mentions": [{"chunk_ref": "chunk-0000", "start_char": 0,
                                      "end_char": 5, "confidence": 0.9}]}
        cp.save("entities", jsons={"entities": [first_entity]})

        def extract(entities, _chunks, _texts, **_kwargs):
            entities.append({"ref": "entity-0002", "text": "Beta Corp",
                             "canonical_form": "beta corp", "type": "ORGANIZATION",
                             "mentions": [{"chunk_ref": "chunk-0000", "start_char": 15,
                                           "end_char": 24, "confidence": 0.6}]})
            return ([{"source_entity_ref": "entity-0001",
                     "target_entity_ref": "entity-0002", "type": "works_at",
                     "evidence_chunk_refs": ["chunk-0000"],
                     "extractor": "test", "strength": 0.7}], False)

        monkeypatch.setattr(runner, "_extract_real_relationships", extract)
        fresh = runner._real_pipeline(req, _workdir(tmp_path, "fresh"))
        assert len(fresh["entities"]) == 2

        def should_not_extract(*_args, **_kwargs):
            raise AssertionError("relationships should have resumed")

        monkeypatch.setattr(runner, "_extract_real_relationships", should_not_extract)
        resumed = runner._real_pipeline(req, _workdir(tmp_path, "resumed"))
        assert resumed["manifest"]["phase_reuse"]["relationships"] == "reused"
        refs = {e["ref"] for e in resumed["entities"]}
        assert refs == {"entity-0001", "entity-0002"}
        assert all(r["source_entity_ref"] in refs and r["target_entity_ref"] in refs
                   for r in resumed["entity_relationships"])

        # A checkpoint from the old build has valid relationships and a
        # valid marker, but never saved the post-relationship entities.
        entity_payload = cp.root / "relationships" / "entities.json"
        entity_payload.unlink()
        marker_path = cp.root / "relationships.marker.json"
        marker = json.loads(marker_path.read_text(encoding="utf-8"))
        marker["files"].pop("entities.json")
        marker_path.write_text(json.dumps(marker), encoding="utf-8")
        monkeypatch.setattr(runner, "_extract_real_relationships", extract)
        recovered = runner._real_pipeline(req, _workdir(tmp_path, "legacy"))
        assert recovered["manifest"]["phase_reuse"]["relationships"] == "computed"
        recovered_refs = {e["ref"] for e in recovered["entities"]}
        assert recovered_refs == {"entity-0001", "entity-0002"}
    finally:
        settings.set(old)
