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
    """Returns canned spans per text; tracks calls."""

    def __init__(self, spans_per_call):
        self.spans_per_call = spans_per_call
        self.calls = 0

    def predict_entities(self, text, labels, threshold=0.45, multi_label=True):
        self.calls += 1
        return self.spans_per_call.get(text, [])


def _patch_gliner(monkeypatch, spans, model=None):
    fake = model if model is not None else _FakeGliner(spans)

    class _G:
        @staticmethod
        def from_pretrained(_name):
            return fake

    # _get_gliner caches module-globally — reset per test (monkeypatch
    # restores the previous value at teardown, keeping tests isolated).
    monkeypatch.setattr(runner, "_GLINER_MODEL", None)
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
    return fake


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
        # end_char = start + len(text); start_char is the ABSOLUTE span
        # offset from GLiNER ("Alpha works at " is 15 chars)
        m = entities[0]["mentions"][0]
        assert m["start_char"] == 15
        assert m["end_char"] == 15 + len("Beta Corp")

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

    def test_span_missing_keys_skipped_not_fatal(self, monkeypatch):
        # W4: spans without text/label keys (or non-str values) are model
        # output too — skip, don't kill the job.
        _patch_gliner(monkeypatch, {
            "t": [
                {"label": "organization", "start": 0, "score": 0.9},  # no text
                {"text": "No Label Org", "start": 0, "score": 0.9},   # no label
                {"text": 12345, "label": "organization", "start": 0, "score": 0.9},  # non-str
                {"text": "Valid Org", "label": "organization",
                 "start": 0, "score": 0.9},
            ],
        })
        entities = runner._extract_real_entities([("chunk-0001", "t")])
        assert [e["text"] for e in entities] == ["Valid Org"]

    def test_model_failure_skips_chunk_not_job(self, monkeypatch):
        # W3: one exploding chunk must not kill the whole job; other chunks
        # still get processed.
        class _FlakyGliner:
            def __init__(self):
                self._ok = _FakeGliner({"good chunk": [
                    {"text": "Valid Org", "label": "organization",
                     "start": 0, "score": 0.9},
                ]})

            def predict_entities(self, text, labels, threshold=0.45,
                                 multi_label=True):
                if "bad" in text:
                    raise RuntimeError("model exploded")
                return self._ok.predict_entities(text, labels, threshold,
                                                 multi_label)

        _patch_gliner(monkeypatch, None, model=_FlakyGliner())
        entities = runner._extract_real_entities(
            [("chunk-0001", "bad chunk"), ("chunk-0002", "good chunk")]
        )
        assert [e["text"] for e in entities] == ["Valid Org"]

    def test_whitespace_only_chunk_never_calls_model(self, monkeypatch):
        fake = _patch_gliner(monkeypatch, {})
        entities = runner._extract_real_entities([("chunk-0001", "   \n  ")])
        assert entities == []
        assert fake.calls == 0

    def test_case_insensitive_grouping_across_chunks(self, monkeypatch):
        _patch_gliner(monkeypatch, {
            "Beta Corp announced": [
                {"text": "Beta Corp", "label": "organization", "start": 0, "score": 0.9},
            ],
            "beta corp announced": [
                {"text": "beta corp", "label": "organization", "start": 0, "score": 0.8},
            ],
        })
        entities = runner._extract_real_entities(
            [("chunk-0001", "Beta Corp announced"),
             ("chunk-0002", "beta corp announced")]
        )
        assert len(entities) == 1
        assert len(entities[0]["mentions"]) == 2

    def test_whitespace_normalized_grouping(self, monkeypatch):
        # "Beta  Corp" (double space) and "Beta Corp" unify to one entity
        _patch_gliner(monkeypatch, {
            "Lead": [{"text": "Beta  Corp", "label": "organization",
                      "start": 0, "score": 0.9}],
            "Tail": [{"text": "Beta Corp", "label": "organization",
                      "start": 0, "score": 0.8}],
        })
        entities = runner._extract_real_entities(
            [("chunk-0001", "Lead"), ("chunk-0002", "Tail")]
        )
        assert len(entities) == 1
        assert len(entities[0]["mentions"]) == 2


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

    def test_substring_resolution_and_min_length_floor(self, monkeypatch):
        # W2: "Central Bank" (12 chars) substring-matches the ECB entity;
        # "UN" (2 chars) must NOT substring-match "european central bank"
        # and becomes its own entity instead of mislinking/discarding.
        _patch_mrebel(monkeypatch, [{
            "head": "Central Bank", "head_type": "org",
            "tail": "UN", "tail_type": "org",
            "relation": "part of", "chunk_id": "chunk-0001",
        }])
        entities = [
            {"ref": "entity-0001", "text": "European Central Bank",
             "canonical_form": "european central bank", "type": "ORGANIZATION",
             "description": None, "mentions": []},
        ]
        rels = runner._extract_real_relationships(
            entities, [], {"chunk-0001": "The European Central Bank and the UN met"},
        )
        assert rels[0]["source_entity_ref"] == "entity-0001"
        assert rels[0]["target_entity_ref"] == "entity-0002"
        assert entities[1]["text"] == "UN"

    def test_rtype_normalization_and_dedup_includes_type(self, monkeypatch):
        # "part of" → "part_of"; newline collapses too; same endpoints with
        # different relations are NOT deduped (dedup key includes type).
        _patch_mrebel(monkeypatch, [
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "part of", "chunk_id": "chunk-0001"},
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "acquires\nnow", "chunk_id": "chunk-0001"},
        ])
        rels = runner._extract_real_relationships(
            self._entities(), [], {"chunk-0001": "Beta Corp Gamma Ltd"},
        )
        assert len(rels) == 2
        assert {r["type"] for r in rels} == {"part_of", "acquires_now"}

    def test_duplicate_evidence_same_chunk_not_duplicated(self, monkeypatch):
        _patch_mrebel(monkeypatch, [
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "acquires", "chunk_id": "chunk-0001"},
            {"head": "Beta Corp", "head_type": "org", "tail": "Gamma Ltd",
             "tail_type": "org", "relation": "acquires", "chunk_id": "chunk-0001"},
        ])
        rels = runner._extract_real_relationships(
            self._entities(), [], {"chunk-0001": "Beta Corp Gamma Ltd"},
        )
        assert len(rels) == 1
        assert rels[0]["evidence_chunk_refs"] == ["chunk-0001"]

    def test_by_text_whitespace_normalization_exact_match(self, monkeypatch):
        # Fix 7: entity stored with irregular whitespace ("Beta  Corp")
        # resolves exactly from the clean head "Beta Corp".
        _patch_mrebel(monkeypatch, [{
            "head": "Beta Corp", "head_type": "org", "tail": "Omega SE",
            "tail_type": "org", "relation": "acquires", "chunk_id": "chunk-0001",
        }])
        entities = [{
            "ref": "entity-0001", "text": "Beta  Corp",
            "canonical_form": "beta corp", "type": "ORGANIZATION",
            "description": None, "mentions": [],
        }]
        rels = runner._extract_real_relationships(
            entities, [], {"chunk-0001": "Beta  Corp buys Omega SE"},
        )
        assert rels[0]["source_entity_ref"] == "entity-0001"


class TestAssignContractChunkIds:
    """6a: the chunk_id overwrite is prod-only code — test it directly."""

    def test_overwrites_chunker_ids_preserving_siblings(self):
        chunks = [
            {"text": "a", "metadata": {"chunk_id": "job1_chunk_0000",
                                      "page_start": "3"}},
            {"text": "b", "metadata": {"chunk_id": "job1_chunk_0001",
                                      "page_start": "4"}},
        ]
        runner._assign_contract_chunk_ids(chunks)
        assert [c["metadata"]["chunk_id"] for c in chunks] == [
            "chunk-0000", "chunk-0001",
        ]
        assert chunks[0]["metadata"]["page_start"] == "3"

    def test_creates_or_repairs_missing_metadata(self):
        # Lost-update guard: missing or None metadata must not silently
        # drop the write into a temp dict.
        chunks = [{"text": "a"}, {"text": "b", "metadata": None}]
        runner._assign_contract_chunk_ids(chunks)
        assert chunks[0]["metadata"] == {"chunk_id": "chunk-0000"}
        assert chunks[1]["metadata"] == {"chunk_id": "chunk-0001"}


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
