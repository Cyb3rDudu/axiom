#!/usr/bin/env python
"""Deprecated alias: the base script is fully device-parametrized since #174.
Kept so historical carrier runbooks keep working; forwards to pydense_ref.py
with --device cuda (the old positional device arg, if given, still works).

Usage: pydense_ref_cuda.py <sample_chunks.json> <go_run.bin> <outdir> [device]
"""
import os
import subprocess
import sys

here = os.path.dirname(os.path.abspath(__file__))
cmd = [sys.executable, os.path.join(here, "pydense_ref.py"),
       *sys.argv[1:3], sys.argv[3] if len(sys.argv) > 3 else "/tmp",
       "--device", sys.argv[4] if len(sys.argv) > 4 else "cuda"]
sys.exit(subprocess.run(cmd).returncode)
