"""Wire protocol for GPU worker RPC.

Format: [4 bytes length, big-endian][msgpack payload]

Request:  {"method": str, "args": dict, "id": str}
Response: {"id": str, "ok": bool, "result": Any | "error": str, "traceback": str}
"""

import struct
import socket
from typing import Any

import msgpack
import msgpack_numpy

# Register numpy array support once at import time.
msgpack_numpy.patch()


class ProtocolError(Exception):
    """Raised for any protocol-level failure (bad frame, connection closed)."""


def recv_exact(sock: socket.socket, n: int) -> bytes:
    """Read exactly n bytes from sock or raise ProtocolError on short read."""
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ProtocolError(f"connection closed after {len(buf)}/{n} bytes")
        buf.extend(chunk)
    return bytes(buf)


def send_frame(sock: socket.socket, payload: Any) -> None:
    """Serialize payload as msgpack and send length-prefixed."""
    data = msgpack.packb(payload, use_bin_type=True)
    sock.sendall(struct.pack(">I", len(data)) + data)


def recv_frame(sock: socket.socket) -> Any:
    """Receive a length-prefixed msgpack frame and return the decoded object."""
    size_bytes = recv_exact(sock, 4)
    (size,) = struct.unpack(">I", size_bytes)
    payload = recv_exact(sock, size)
    return msgpack.unpackb(payload, raw=False)


def make_request(method: str, args: dict, req_id: str) -> dict:
    return {"method": method, "args": args, "id": req_id}


def make_response_ok(req_id: str, result: Any) -> dict:
    return {"id": req_id, "ok": True, "result": result}


def make_response_error(req_id: str, error: str, traceback_str: str = "") -> dict:
    return {"id": req_id, "ok": False, "error": error, "traceback": traceback_str}
