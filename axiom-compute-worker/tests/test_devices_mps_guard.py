"""#382: mps is Darwin-only — the per-model device resolution must never
hand an Apple-host env leftover (DEVICE_GLINER=mps observed on a
Linux/ROCm box) to torch verbatim on non-Darwin systems.
"""
import pytest

# devices.py imports torch at module top; the light CI leg runs without it.
pytest.importorskip("torch")

from axiom_compute_worker.compute_core.devices import HardwareDetector


def test_mps_assignment_degrades_to_cpu_on_non_darwin(monkeypatch, caplog):
    det = HardwareDetector()
    if det.system == "Darwin":
        pytest.skip("guard semantics differ on Darwin (mps is legal there)")
    monkeypatch.setenv("DEVICE_GLINER", "mps")
    with caplog.at_level("WARNING"):
        device = det.get_model_device("gliner")
    assert device == "cpu"
    assert "Darwin-only" in caplog.text


def test_explicit_cuda_env_is_untouched_by_the_guard(monkeypatch):
    det = HardwareDetector()
    monkeypatch.setenv("DEVICE_GLINER", "cuda")
    # no exception, no degradation branch — the value resolves as given
    # (torch may not have it; get_model_device does not probe).
    assert det.get_model_device("gliner") == "cuda"
