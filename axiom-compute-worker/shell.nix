# axiom-compute-worker heavy-compute environment (Gate 5).
#
# Builds a python311 venv with the real-compute-backend dependencies:
# marker-pdf==1.10.2 (last v1 before the v2 jump — proven in ~/Code/marker-poc),
# FlagEmbedding + gliner + transformers for embeddings/entities/relations.
# Apple MPS GPU acceleration is used via torch (torch.backends.mps available).
#
# Usage:
#   nix-shell axiom-compute-worker/shell.nix
#   # inside the shell, the venv is auto-created/activated; first run installs deps:
#   pip install -r requirements-heavy.txt
#   # then run the processor with the real backend:
#   AXIOM_PROCESSOR_COMPUTE=real python -m axiom_compute_worker  # (legacy: python -m axiom_ng_runner)
#
# Based on ~/Code/marker-poc/shell.nix (dudu's verified POC).
{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  buildInputs = [
    pkgs.python311
    pkgs.python311Packages.pip
    pkgs.python311Packages.virtualenv
    pkgs.libpng
    pkgs.zlib
    pkgs.pkg-config
    pkgs.fontconfig
  ];

  shellHook = ''
    if [ ! -d "$PWD/.venv" ]; then
      python3 -m venv "$PWD/.venv"
    fi
    source "$PWD/.venv/bin/activate"
    python --version
    echo "Heavy venv ready. Install deps: pip install -r requirements-heavy.txt"
  '';
}
