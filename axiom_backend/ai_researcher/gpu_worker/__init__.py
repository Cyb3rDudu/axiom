"""GPU worker subprocess for isolated inference.

The main backend process stays alive indefinitely. GPU models (embedder,
reranker, GLiNER) run in a child subprocess that can die on idle and
respawn on demand without affecting FastAPI, DB connections, or
WebSockets.
"""
