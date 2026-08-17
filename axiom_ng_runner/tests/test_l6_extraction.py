"""Tests for L6 real entity/relationship extraction wiring (GLiNER + mREBEL).

sys.modules stubs — no heavy deps needed, runs in both venvs. Evidence power:
- grouping/dedup logic breaks → red
- ref resolution (exact/substring/new-entity) breaks → red
- conditional fill in _build_reference_result breaks → red
"""

import sys
import types
from pathlib import Path

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
    # entity_extractor's import pulls the package __init__ chain →
    # numpy. Stub it with the constants the code reads.
    import re as _re

    pkg = types.ModuleType("axiom_ng_runner")
    core = types.ModuleType("axiom_ng_runner.compute_core")
    ee = types.ModuleType("axiom_ng_runner.compute_core.entity_extractor")
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
    monkeypatch.setitem(sys.modules, "axiom_ng_runner", pkg)
    monkeypatch.setitem(sys.modules, "axiom_ng_runner.compute_core", core)
    monkeypatch.setitem(sys.modules, "axiom_ng_runner.compute_core.entity_extractor", ee)
    return fake


def _patch_mrebel(monkeypatch, triples):
    if callable(triples):
        fn = triples  # already a fake extract function
    else:

        def fn(chunks):
            return triples

    mod = types.ModuleType("axiom_ng_runner.compute_core.relation_extractor")
    mod.__dict__.update({"extract_relations_from_chunks": fn})
    # Stub parents so the from-import never touches the real __init__
    # (which pulls numpy/torch).
    pkg = types.ModuleType("axiom_ng_runner")
    core = types.ModuleType("axiom_ng_runner.compute_core")
    pkg.__dict__.update({"core_rag": core})
    monkeypatch.setitem(sys.modules, "axiom_ng_runner", pkg)
    monkeypatch.setitem(sys.modules, "axiom_ng_runner.compute_core", core)
    monkeypatch.setitem(sys.modules, "axiom_ng_runner.compute_core.relation_extractor", mod)


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
    def _result(request, chunk_dicts=None, **kwargs):
        import pathlib
        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".md", mode="w", delete=False) as f:
            f.write("# t\n\ntext")
            path = pathlib.Path(f.name)
        return runner._build_reference_result(
            request=request,
            work_dir=pathlib.Path(tempfile.mkdtemp()),
            chunk_dicts=chunk_dicts or [{"text": "Some Capitalized Text", "metadata": {}}],
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
        # No real embeddings in chunk_dicts → stub model name (honesty fix:
        # dense name is data-driven, same variant-b detection as the fill).
        assert result["processor"]["models"]["dense_embedding"] == "reference-bge-m3"
        assert result["processor"]["models"]["relationship_extraction"] == "reference-mrebel"

    def test_real_dense_embeddings_report_real_model(self):
        # Known Gap Carrier-POC: chunks carrying REAL TextEmbedder vectors
        # must report BAAI/bge-m3, not the reference stub name.
        req = self._request()
        req["processing"]["compute_dense_embeddings"] = True
        result = self._result(
            req,
            chunk_dicts=[{
                "text": "Some Capitalized Text",
                "metadata": {},
                # REAL TextEmbedder shape: dense is a plain float list
                # (embedder.py:221 — .tolist()), not a contract dict.
                "embeddings": {"dense": [0.1, 0.2, 0.3, 0.4]},
            }],
        )
        assert (result["processor"]["models"]["dense_embedding"]
                == "BAAI/bge-m3")
        # And the real vectors pass through untouched (variant b: the stub
        # fill must not overwrite them).
        dense = result["chunks"][0]["embeddings"]["dense"]
        assert list(dense["values"]) == [0.1, 0.2, 0.3, 0.4]

    def test_real_takes_precedence_even_when_empty(self):
        # Empty real extraction (0 entities) must NOT trigger the stub
        result = self._result(self._request(), real_entities=[], real_relationships=[])
        assert result["entities"] == []
        assert result["entity_relationships"] == []
        assert (result["processor"]["models"]["entity_extraction"]
                == "urchade/gliner_multi-v2.1")
        assert (result["processor"]["models"]["relationship_extraction"]
                == "Babelscape/mrebel-large")


class TestPipelineWiring:
    """Integration: _real_pipeline must CALL the real extractors and pass
    their results through — the exact gap that enabled the embeddings
    fake-fix (helper tested in isolation, call site severed silently).

    Mutation-bar: severing the _real_pipeline→_extract_real_entities wiring
    (real_entities = None) MUST turn this red.
    """

    CHUNK_TEXT = "Alpha works at Beta Corp"

    @staticmethod
    def _request(tmp_path):
        src = tmp_path / "doc.pdf"
        src.write_bytes(b"%PDF-1.4 fake")
        return {
            "job_id": "job-wire-1",
            "attachment": {
                "attachment_id": "att-wire-1",
                "local_path": str(src),
                "content_type": "application/pdf",
            },
            "processing": {
                "extract_entities": True,
                "extract_relationships": True,
                "compute_dense_embeddings": False,
                "compute_sparse_embeddings": False,
            },
        }

    @staticmethod
    def _patch_pipeline_heavies(monkeypatch, tmp_path, chunk_text):
        import subprocess as _subprocess

        def fake_run(cmd, **_kwargs):
            # cmd: [python, -m, pdf_worker, src, out_md, out_images]
            out_md_path = Path(cmd[4])
            out_md_path.write_text(f"# T\n\n{chunk_text}", encoding="utf-8")
            out = types.SimpleNamespace(
                returncode=0,
                stdout='{"image_mapping": {}}',
                stderr="",
            )
            return out

        monkeypatch.setattr(_subprocess, "run", fake_run)

        # extract_page_labels stub (compute_core.pdf_processing)
        proc_mod = types.ModuleType("axiom_ng_runner.compute_core.pdf_processing")
        proc_mod.__dict__.update({"extract_page_labels": lambda _p: {1: "1"}})
        monkeypatch.setitem(
            sys.modules, "axiom_ng_runner.compute_core.pdf_processing", proc_mod
        )

        # page_trust stub (#173): the real path imports build_page_trust —
        # under the stubbed core it cannot resolve (no __path__), so provide
        # the trust tuple the runner expects (labels + honest levels).
        trust_mod = types.ModuleType("axiom_ng_runner.compute_core.page_trust")
        trust_mod.__dict__.update({
            "build_page_trust": lambda _p: ({1: "1"}, {1: "pdf_label_sane"}),
            "PHYSICAL_ONLY": "physical_only",
            "NONE": "none",
        })
        monkeypatch.setitem(
            sys.modules, "axiom_ng_runner.compute_core.page_trust", trust_mod
        )

        # Chunker stub emitting REAL Chunker-shaped dicts (chunk_id in the
        # {doc_id}_chunk_{i:04d} format that _assign_contract_chunk_ids
        # must replace before mREBEL reads it).
        class _Chunker:
            def __init__(self, max_chunk_tokens=1200):
                pass

            def chunk(self, _markdown, doc_metadata=None):
                did = (doc_metadata or {}).get("doc_id", "doc")
                return [{
                    "text": chunk_text,
                    "metadata": {
                        "chunk_id": f"{did}_chunk_0001",
                        "image_refs": [],
                    },
                }]

        chunker_mod = types.ModuleType("axiom_ng_runner.compute_core.chunker")
        chunker_mod.__dict__.update({"Chunker": _Chunker})
        monkeypatch.setitem(sys.modules, "axiom_ng_runner.compute_core.chunker", chunker_mod)

    def test_real_pipeline_uses_real_extractors(self, monkeypatch, tmp_path):
        import pathlib

        _patch_gliner(monkeypatch, {
            self.CHUNK_TEXT: [
                {"text": "Beta Corp", "label": "organization",
                 "start": 15, "score": 0.9},
            ],
        })

        def _fake_extract(chunks):
            # Realistic: read the chunk_id the wiring assigned (proves
            # _assign_contract_chunk_ids ran — the Chunker stub emitted
            # "{doc_id}_chunk_0001" which would NOT resolve).
            cid = chunks[0]["metadata"]["chunk_id"]
            return [{
                "head": "Beta Corp", "head_type": "org",
                "tail": "NewCo", "tail_type": "org",
                "relation": "acquires", "chunk_id": cid,
            }]

        _patch_mrebel(monkeypatch, _fake_extract)
        self._patch_pipeline_heavies(monkeypatch, tmp_path, self.CHUNK_TEXT)

        work = pathlib.Path(tmp_path) / "work"
        work.mkdir(parents=True, exist_ok=True)  # app.py:155 does this
        result = runner._real_pipeline(self._request(tmp_path), work)

        # The real path ran (not reference stubs)
        assert (result["processor"]["models"]["entity_extraction"]
                == "urchade/gliner_multi-v2.1"), "real GLiNER path not taken"
        assert (result["processor"]["models"]["relationship_extraction"]
                == "Babelscape/mrebel-large"), "real mREBEL path not taken"

        # Entities come from the GLiNER stub, not the reference regex
        # (regex stub would emit type=METHOD capitalized tokens).
        ents = {e["text"]: e for e in result["entities"]}
        assert "Beta Corp" in ents
        assert ents["Beta Corp"]["type"] == "ORGANIZATION"
        assert ents["Beta Corp"]["mentions"][0]["chunk_ref"] == "chunk-0000"

        # The mREBEL relationship came through with CONTRACT-format evidence
        # (0-based chunk-0000, NOT the Chunker stub's job-wire-1_chunk_0001).
        rels = result["entity_relationships"]
        assert len(rels) == 1
        assert rels[0]["evidence_chunk_refs"] == ["chunk-0000"]
        assert rels[0]["source_entity_ref"] == ents["Beta Corp"]["ref"]

    def test_severed_wiring_falls_back_and_is_detectable(
        self, monkeypatch, tmp_path
    ):
        """Mutation-bar proof: kill the pipeline→extractor wiring and the
        integration assertions above break. Simulated here by driving the
        same pipeline WITHOUT the GLiNER/mREBEL stubs' data flowing: the
        reference fallback must produce visibly different (detectable)
        output — models field flips to reference-gliner."""
        import pathlib

        _patch_gliner(monkeypatch, {})  # model yields nothing
        _patch_mrebel(monkeypatch, [])
        self._patch_pipeline_heavies(monkeypatch, tmp_path, self.CHUNK_TEXT)

        work = pathlib.Path(tmp_path) / "work"
        work.mkdir(parents=True, exist_ok=True)  # app.py:155 does this
        result = runner._real_pipeline(self._request(tmp_path), work)
        # Real path still taken (extraction requested, real list = [])
        assert (result["processor"]["models"]["entity_extraction"]
                == "urchade/gliner_multi-v2.1")
        # Empty real extraction stays empty (no silent stub fill)
        assert result["entities"] == []
        assert result["entity_relationships"] == []
