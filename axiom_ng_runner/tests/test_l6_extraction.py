"""Tests for L6 real entity/relationship extraction wiring (GLiNER + mREBEL).

sys.modules stubs — no heavy deps needed, runs in both venvs. Evidence power:
- grouping/dedup logic breaks → red
- ref resolution (exact/substring/new-entity) breaks → red
- conditional fill in _build_reference_result breaks → red
"""

import sys
import types

from axiom_ng_runner import runner


class _FakeGliner:
    """Returns canned spans per text."""

    def __init__(self, spans_per_call):
        self.spans_per_call = spans_per_call

    def predict_entities(self, text, labels, threshold=0.45, multi_label=True):
        return self.spans_per_call.get(text, [])


def _patch_gliner(monkeypatch, spans):
    fake = _FakeGliner(spans)

    class _G:
        @staticmethod
        def from_pretrained(_name):
            return fake

    mod = types.ModuleType("gliner")
    mod.__dict__.update({"GLiNER": _G})
    monkeypatch.setitem(sys.modules, "gliner", mod)
    # entity_extractor's import pulls ai_researcher.core_rag.__init__ →
    # numpy. Stub it with the constants the code reads.
    import re as _re

    pkg = types.ModuleType("ai_researcher")
    core = types.ModuleType("ai_researcher.core_rag")
    ee = types.ModuleType("ai_researcher.core_rag.entity_extractor")
    ee.__dict__.update({
        "GLINER_LABELS": ["person", "organization", "concept"],
        "_GLINER_TYPE_MAP": {
            "person": "PERSON", "organization": "ORGANIZATION",
            "concept": "CONCEPT",
        },
        "_NOISE_RE": _re.compile(r"\bet\s+al\.?$", _re.IGNORECASE),
        "_GENERIC_WORDS": frozenset({"firm", "government"}),
    })
    pkg.__dict__.update({"core_rag": core})
    monkeypatch.setitem(sys.modules, "ai_researcher", pkg)
    monkeypatch.setitem(sys.modules, "ai_researcher.core_rag", core)
    monkeypatch.setitem(sys.modules, "ai_researcher.core_rag.entity_extractor", ee)


def _patch_mrebel(monkeypatch, triples):
    mod = types.ModuleType("ai_researcher.core_rag.relation_extractor")
    mod.__dict__.update(
        {"extract_relations_from_chunks": lambda chunks: triples}
    )
    # Stub parents so the from-import never touches the real __init__
    # (which pulls numpy/torch).
    pkg = types.ModuleType("ai_researcher")
    core = types.ModuleType("ai_researcher.core_rag")
    pkg.__dict__.update({"core_rag": core})
    monkeypatch.setitem(sys.modules, "ai_researcher", pkg)
    monkeypatch.setitem(sys.modules, "ai_researcher.core_rag", core)
    monkeypatch.setitem(sys.modules, "ai_researcher.core_rag.relation_extractor", mod)


class TestExtractRealEntities:
    def test_groups_same_entity_across_chunks(self, monkeypatch):
        _patch_gliner(monkeypatch, {
            "Alpha works at Beta Corp": [
                {"text": "Beta Corp", "label": "organization", "start": 15, "score": 0.9},
            ],
            "Beta Corp released results": [
                {"text": "Beta Corp", "label": "organization", "start": 0, "score": 0.8},
            ],
        })
        entities = runner._extract_real_entities(
            [("chunk-0001", "Alpha works at Beta Corp"),
             ("chunk-0002", "Beta Corp released results")]
        )
        # One entity, two mentions across chunks
        assert len(entities) == 1
        assert entities[0]["ref"] == "entity-0001"
        assert entities[0]["type"] == "ORGANIZATION"
        assert len(entities[0]["mentions"]) == 2
        assert entities[0]["mentions"][0]["chunk_ref"] == "chunk-0001"
        assert entities[0]["mentions"][1]["chunk_ref"] == "chunk-0002"
        # end_char = start + len(text)
        m = entities[0]["mentions"][0]
        assert m["end_char"] == m["start_char"] + len("Beta Corp")

    def test_different_types_are_separate_entities(self, monkeypatch):
        _patch_gliner(monkeypatch, {
            "Alpha text": [
                {"text": "Alpha", "label": "person", "start": 0, "score": 0.9},
                {"text": "Alpha", "label": "concept", "start": 0, "score": 0.7},
            ],
        })
        entities = runner._extract_real_entities([("chunk-0001", "Alpha text")])
        assert len(entities) == 2  # PERSON + CONCEPT
        assert {e["type"] for e in entities} == {"PERSON", "CONCEPT"}

    def test_filters_noise_and_generic_words(self, monkeypatch):
        _patch_gliner(monkeypatch, {
            "t": [
                {"text": "Smith et al.", "label": "person", "start": 0, "score": 0.9},
                {"text": "firm", "label": "concept", "start": 0, "score": 0.9},
                {"text": "X", "label": "concept", "start": 0, "score": 0.9},  # < 2 chars
            ],
        })
        entities = runner._extract_real_entities([("chunk-0001", "t")])
        assert entities == []

    def test_malformed_span_skipped_not_fatal(self, monkeypatch):
        # Trust boundary: model output with bad types must not kill the job.
        _patch_gliner(monkeypatch, {
            "t": [
                {"text": "Beta Corp", "label": "organization", "start": None, "score": 0.9},
                {"text": "Valid Org", "label": "organization", "start": 0, "score": 0.9},
            ],
        })
        entities = runner._extract_real_entities([("chunk-0001", "t")])
        assert [e["text"] for e in entities] == ["Valid Org"]

    def test_unmapped_label_skipped(self, monkeypatch):
        _patch_gliner(monkeypatch, {
            "t": [{"text": "Beta Corp", "label": "date", "start": 0, "score": 0.9}],
        })
        assert runner._extract_real_entities([("chunk-0001", "t")]) == []


class TestExtractRealRelationships:
    @staticmethod
    def _entities():
        return [
            {"ref": "entity-0001", "text": "Beta Corp", "canonical_form": "beta corp",
             "type": "ORGANIZATION", "description": None, "mentions": []},
        ]

    def test_exact_match_new_entity_and_offsets(self, monkeypatch):
        _patch_mrebel(monkeypatch, [{
            "head": "Beta Corp", "head_type": "org",
            "tail": "NewCo", "tail_type": "org",
            "relation": "acquires", "chunk_id": "chunk-0001",
        }])
        entities = self._entities()
        rels = runner._extract_real_relationships(
            entities, [{"metadata": {"chunk_id": "chunk-0001"}}],
            {"chunk-0001": "Beta Corp acquires NewCo today"},
        )
        # head matched exactly; tail created as new entity
        assert len(entities) == 2
        assert rels[0]["source_entity_ref"] == "entity-0001"
        assert rels[0]["target_entity_ref"] == "entity-0002"
        assert rels[0]["type"] == "acquires"
        assert rels[0]["evidence_chunk_refs"] == ["chunk-0001"]
        assert rels[0]["extractor"] == "mrebel-large"
        # New entity mention has real char offsets from text search
        # ("Beta Corp acquires NewCo today".find("NewCo") == 19)
        tail = entities[1]
        assert tail["mentions"][0]["start_char"] == 19
        assert tail["mentions"][0]["end_char"] == 19 + len("NewCo")

    def test_dedup_merges_evidence(self, monkeypatch):
        _patch_mrebel(monkeypatch, [
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "acquires", "chunk_id": "chunk-0001"},
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "acquires", "chunk_id": "chunk-0002"},
        ])
        entities = self._entities()
        rels = runner._extract_real_relationships(
            entities, [],
            {"chunk-0001": "Beta Corp Gamma Ltd", "chunk-0002": "Beta Corp Gamma Ltd"},
        )
        assert len(rels) == 1
        assert rels[0]["evidence_chunk_refs"] == ["chunk-0001", "chunk-0002"]

    def test_self_relationship_skipped(self, monkeypatch):
        _patch_mrebel(monkeypatch, [{
            "head": "Beta Corp", "head_type": "org",
            "tail": "Beta Corp", "tail_type": "org",
            "relation": "part of", "chunk_id": "chunk-0001",
        }])
        rels = runner._extract_real_relationships(self._entities(), [], {})
        assert rels == []

    def test_no_chunk_id_skipped(self, monkeypatch):
        _patch_mrebel(monkeypatch, [{
            "head": "Delta AG", "head_type": "org", "tail": "Omega SE",
            "tail_type": "org", "relation": "acquires", "chunk_id": "",
        }])
        rels = runner._extract_real_relationships(self._entities(), [], {})
        assert rels == []


class TestConditionalFill:
    """_build_reference_result: real params take precedence over stubs."""

    @staticmethod
    def _request():
        return {
            "job_id": "job-1",
            "attachment": {"attachment_id": "att-1", "local_path": "/dev/null",
                           "content_type": "application/pdf"},
            "processing": {"extract_entities": True, "extract_relationships": True},
        }

    @staticmethod
    def _result(request, **kwargs):
        import pathlib
        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".md", mode="w", delete=False) as f:
            f.write("# t\n\ntext")
            path = pathlib.Path(f.name)
        return runner._build_reference_result(
            request=request,
            work_dir=pathlib.Path(tempfile.mkdtemp()),
            chunk_dicts=[{"text": "Some Capitalized Text", "metadata": {}}],
            page_label_map={1: "1"},
            markdown_path=path,
            attachment_id="att-1",
            content_hash="sha256:abc",
            source_page_count=1,
            **kwargs,
        )

    def test_none_falls_back_to_stubs(self):
        result = self._result(self._request())
        assert result["entities"]  # reference stub ran
        assert result["processor"]["models"]["entity_extraction"] == "reference-gliner"
        assert result["processor"]["models"]["relationship_extraction"] == "reference-mrebel"

    def test_real_takes_precedence_even_when_empty(self):
        # Empty real extraction (0 entities) must NOT trigger the stub
        result = self._result(self._request(), real_entities=[], real_relationships=[])
        assert result["entities"] == []
        assert result["entity_relationships"] == []
        assert (result["processor"]["models"]["entity_extraction"]
                == "urchade/gliner_multi-v2.1")
        assert (result["processor"]["models"]["relationship_extraction"]
                == "Babelscape/mrebel-large")
