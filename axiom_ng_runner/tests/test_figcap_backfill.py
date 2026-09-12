"""#268 — figcap backfill engine tests (re-extraction + purge + re-embed)."""

from __future__ import annotations

from axiom_ng_runner.compute_core.figcap_backfill_cli import plan, run


class FakeEmbedder:
    """Deterministic stand-in: dense vector = [len(text)] — proves which text
    was embedded (augmented vs plain)."""

    def embed_chunks(self, chunks):
        for c in chunks:
            c["embeddings"] = {"dense": [float(len(c["text"]))]}


def _chunk(**over):
    c = {
        "chunk_id": "uuid-1",
        "text": "Abb. 3: Übersicht der Finanzierungsformen",
        "section_titles": ["Kapitel"],
        "image_refs": ["image-0000"],
        "image_captions": {},
        "figure_captions": {},
    }
    c.update(over)
    return c


def test_plan_adds_a_missed_caption():
    """A decorated/roman caption the old pattern missed is now added."""
    p = plan([_chunk(figure_captions={})])[0]
    assert p["changed"] is True
    assert p["figure_captions"] == {"image-0000": "Abb. 3: Übersicht der Finanzierungsformen"}


def test_plan_purges_a_prose_false_positive():
    """The stored prose FP (a verb continuation) is purged: new map is empty,
    so the chunk is affected and will be re-embedded without the caption."""
    fp = {"image-0000": "Abb. 4.62 zeigt den Zusammenhang zwischen Aufwand und Ertrag"}
    p = plan([_chunk(text="Abb. 4.62 zeigt den Zusammenhang zwischen Aufwand und Ertrag",
                     figure_captions=fp)])[0]
    assert p["changed"] is True
    assert p["figure_captions"] == {}


def test_plan_unchanged_when_extraction_is_stable():
    """A chunk whose extraction is already correct is NOT re-embedded."""
    cap = {"image-0000": "Abb. 3: Übersicht der Finanzierungsformen"}
    p = plan([_chunk(figure_captions=cap)])[0]
    assert p["changed"] is False
    assert p["figure_captions"] == cap


def test_run_embeds_only_changed_chunks_with_new_augmentation():
    unchanged = _chunk(
        chunk_id="uuid-keep",
        figure_captions={"image-0000": "Abb. 3: Übersicht der Finanzierungsformen"},
    )
    changed = _chunk(chunk_id="uuid-add")
    out = run([unchanged, changed], embedder=FakeEmbedder())
    by_id = {r["chunk_id"]: r for r in out}
    assert by_id["uuid-keep"]["changed"] is False
    assert "values" not in by_id["uuid-keep"]
    assert by_id["uuid-add"]["changed"] is True
    # the embedded text carried the NEW document figure caption label
    assert by_id["uuid-add"]["values"] == [
        float(
            len(
                "Abb. 3: Übersicht der Finanzierungsformen"
                "\n[document figure caption: Abb. 3: Übersicht der Finanzierungsformen]"
            )
        )
    ]


def test_run_no_change_emits_no_vectors():
    cap = {"image-0000": "Abb. 3: Übersicht der Finanzierungsformen"}
    out = run([_chunk(figure_captions=cap)], embedder=FakeEmbedder())
    assert out == [
        {
            "chunk_id": "uuid-1",
            "figure_captions": cap,
            "changed": False,
        }
    ]


def test_engine_failure_is_loud():
    """A broken embedder must raise BEFORE emitting a plan — no partial
    application."""

    class Boom:
        def embed_chunks(self, chunks):
            raise RuntimeError("backend down")

    import pytest

    with pytest.raises(RuntimeError):
        run([_chunk()], embedder=Boom())
