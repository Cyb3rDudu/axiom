"""Shared device selection for the feasibility reference scripts (#174).

Policy (mirrors compute_core/devices.py runner policy): auto = CUDA if
available (fp16), else MPS (fp32), else CPU. A forced --device keeps the
same fp16 policy unless --no-fp16 is given (parity references historically
ran fp32, so old numbers reproduce with --no-fp16).

Every script logs the chosen device and fp16 mode at startup; nothing under
cmd/ hardcodes a device string anymore.
"""
from __future__ import annotations

import argparse
import shutil
import subprocess
import sys


def add_device_args(parser: argparse.ArgumentParser) -> None:
    g = parser.add_argument_group("device selection")
    g.add_argument(
        "--device",
        default="auto",
        help="auto (best available: CUDA>fp16, MPS, CPU) or a forced device "
        "(cuda, cuda:0, mps, cpu — comma lists allowed where the library "
        "supports them) for parity comparisons",
    )
    g.add_argument(
        "--no-fp16",
        action="store_true",
        help="force fp32 even on CUDA (reproduces the historical fp32 parity numbers)",
    )


def _cuda_available() -> bool:
    try:
        import torch

        return bool(torch.cuda.is_available())
    except Exception:  # noqa: BLE001 — probe must never crash the caller (torch absent → smi fallback)
        # nvidia-smi fallback for environments without torch on PATH
        return bool(shutil.which("nvidia-smi")) and subprocess.run(
            ["nvidia-smi", "-L"], capture_output=True, check=False
        ).returncode == 0


def _mps_available() -> bool:
    try:
        import torch

        return bool(getattr(torch.backends, "mps", None) and torch.backends.mps.is_available())
    except Exception:  # noqa: BLE001 — probe must never crash the caller (no MPS backend → False)
        return False


def pick_device(device_arg: str, *, no_fp16: bool = False, label: str = "") -> tuple[str, bool]:
    """Resolve --device to (device, use_fp16) and log the decision."""
    use_fp16 = not no_fp16
    if device_arg == "auto":
        if _cuda_available():
            device, fp16 = "cuda", use_fp16
        elif _mps_available():
            device, fp16 = "mps", False
        else:
            device, fp16 = "cpu", False
        chosen = f"auto -> {device}"
    else:
        device, fp16 = device_arg, (use_fp16 and device_arg.startswith("cuda"))
        chosen = device_arg
    print(f"[device{'/' + label if label else ''}] {chosen}, fp16={'on' if fp16 else 'off'}",
          flush=True)
    return device, fp16


def ort_providers(device: str) -> list[str]:
    """Map a resolved device to onnxruntime providers (ORT has no MPS EP)."""
    if device.startswith("cuda"):
        return ["CUDAExecutionProvider", "CPUExecutionProvider"]
    return ["CPUExecutionProvider"]


def main() -> int:  # smoke self-check: `python _device.py --device cpu`
    p = argparse.ArgumentParser()
    add_device_args(p)
    args = p.parse_args()
    dev, _ = pick_device(args.device, no_fp16=args.no_fp16, label="selftest")
    print(f"providers would be: {ort_providers(dev)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
