#!/usr/bin/env python3
"""Bridge Docker's published companion port to the existing loopback service.

Docker publishes this listener only as 127.0.0.1:60531. The native companion
keeps its own IPv4-loopback binding and browser admission rules. This adapter
does not expose Agent routes, credentials, arbitrary URLs, or a general proxy.
"""
from __future__ import annotations

from collections import Counter
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import signal
import threading
from urllib.parse import urlsplit


PUBLIC_HOST = "127.0.0.1:60531"
TRUSTED_ORIGIN = "http://127.0.0.1:60511"
MAX_REQUEST_BYTES = 20 * 1024 * 1024
MAX_RESPONSE_BYTES = 32 * 1024 * 1024
GET_PATHS = frozenset({"/ime/state", "/attachments/state", "/attachments/receipt",
                       "/voice/state", "/voice/status"})
POST_PATHS = frozenset({"/ime/lease", "/ime/preedit", "/attachments/lease",
                        "/attachments/upload", "/voice/transcribe",
                        "/voice/action", "/voice/synthesize", "/voice/lease"})
FORWARD_HEADERS = frozenset({"origin", "content-type", "content-length",
    "access-control-request-method", "access-control-request-headers",
    "x-meadow-voice", "x-meadow-voice-epoch", "x-meadow-voice-client",
    "x-meadow-ime", "x-meadow-ime-client", "x-meadow-attachments",
    "x-meadow-attachments-client", "x-meadow-attachments-meta"})
RESPONSE_HEADERS = frozenset({"content-type", "access-control-allow-origin", "vary",
    "cache-control", "x-content-type-options", "access-control-allow-methods",
    "access-control-allow-headers", "access-control-max-age"})


class CompanionProxy(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = False

    def __init__(self, address=("0.0.0.0", 6081), *, upstream_port=60531):
        if address[0] not in ("0.0.0.0", "127.0.0.1"):
            raise ValueError("The Docker companion listener requires IPv4")
        if not 1 <= upstream_port <= 65535:
            raise ValueError("Invalid loopback upstream port")
        self.upstream_port = upstream_port
        self.forward_slots = threading.BoundedSemaphore(4)
        super().__init__(address, CompanionHandler)


class Rejected(Exception):
    def __init__(self, status, code):
        self.status, self.code = status, code


class CompanionHandler(BaseHTTPRequestHandler):
    server_version = "MeadowCompanionProxy/1"

    def log_message(self, *_args):
        pass

    def setup(self):
        super().setup()
        self.connection.settimeout(15)

    def _respond(self, status, payload, headers=None):
        self.close_connection = True
        self.send_response(status)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    def _error(self, status, code):
        headers = {"Content-Type": "application/json; charset=utf-8",
                   "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}
        if self.headers.get("Origin") == TRUSTED_ORIGIN:
            headers.update({"Access-Control-Allow-Origin": TRUSTED_ORIGIN, "Vary": "Origin"})
        self._respond(status, json.dumps({"error": code}).encode(), headers)

    def _admit(self):
        names = Counter(name.lower() for name in self.headers.keys())
        if any(count != 1 for count in names.values()):
            raise Rejected(400, "duplicate_header")
        if sum(len(name) + len(value) for name, value in self.headers.items()) > 64 * 1024:
            raise Rejected(431, "headers_too_large")
        if any(len(value) > 8192 for name, value in self.headers.items()
               if name.lower() in FORWARD_HEADERS):
            raise Rejected(431, "header_too_large")
        if self.headers.get("Host") != PUBLIC_HOST:
            raise Rejected(403, "forbidden_host")
        if self.headers.get("Origin") != TRUSTED_ORIGIN:
            raise Rejected(403, "forbidden_origin")
        if "Transfer-Encoding" in self.headers:
            raise Rejected(400, "unsupported_transfer_encoding")
        if len(self.path) > 4096:
            raise Rejected(414, "request_target_too_long")
        target = urlsplit(self.path)
        if target.scheme or target.netloc or target.fragment or not self.path.startswith("/"):
            raise Rejected(400, "invalid_request_target")
        allowed = GET_PATHS if self.command == "GET" else POST_PATHS
        if self.command == "OPTIONS":
            allowed = GET_PATHS | POST_PATHS
        if target.path not in allowed:
            raise Rejected(404, "unknown_companion_route")
        if target.query and target.path != "/attachments/receipt":
            raise Rejected(400, "unexpected_query")
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            if self.command == "POST":
                raise Rejected(411, "content_length_required")
            length = 0
        elif len(raw_length) > 9 or not raw_length.isascii() or not raw_length.isdecimal():
            raise Rejected(400, "invalid_content_length")
        else:
            length = int(raw_length)
        limit = MAX_REQUEST_BYTES if target.path == "/attachments/upload" else 64 * 1024
        if target.path in ("/ime/lease", "/ime/preedit", "/attachments/lease"):
            limit = 8192
        elif target.path == "/voice/transcribe":
            limit = 16000 * 2 * 30 + 4096
        if length > limit:
            raise Rejected(413, "body_too_large")
        if (self.command == "POST" and length < 1) or (self.command != "POST" and length != 0):
            raise Rejected(400, "invalid_body_length")
        return length

    def _forward(self):
        connection = None
        acquired = False
        try:
            length = self._admit()
            acquired = self.server.forward_slots.acquire(blocking=False)
            if not acquired:
                raise Rejected(429, "companion_busy")
            body = self.rfile.read(length) if length else None
            if length and len(body) != length:
                raise Rejected(400, "truncated_body")
            headers = {name: value for name, value in self.headers.items()
                       if name.lower() in FORWARD_HEADERS}
            # The upstream service validates Host against its actual listener.
            headers["Host"] = f"127.0.0.1:{self.server.upstream_port}"
            headers["Connection"] = "close"
            connection = http.client.HTTPConnection("127.0.0.1", self.server.upstream_port, timeout=100)
            connection.request(self.command, self.path, body=body, headers=headers)
            response = connection.getresponse()
            declared = response.getheader("Content-Length")
            if declared is not None and (not declared.isdecimal() or int(declared) > MAX_RESPONSE_BYTES):
                raise Rejected(502, "invalid_companion_response")
            payload = response.read(MAX_RESPONSE_BYTES + 1)
            if len(payload) > MAX_RESPONSE_BYTES or (declared is not None and len(payload) != int(declared)):
                raise Rejected(502, "invalid_companion_response")
            response_headers = {name: value for name, value in response.getheaders()
                                if name.lower() in RESPONSE_HEADERS}
            self._respond(response.status, payload, response_headers)
        except Rejected as error:
            self._error(error.status, error.code)
        except (OSError, http.client.HTTPException, ValueError):
            self._error(502, "companion_unavailable")
        finally:
            if connection is not None:
                connection.close()
            if acquired:
                self.server.forward_slots.release()

    do_GET = _forward
    do_POST = _forward
    do_OPTIONS = _forward

    def _unsupported(self):
        self._error(405, "method_not_allowed")

    do_HEAD = _unsupported
    do_PUT = _unsupported
    do_PATCH = _unsupported
    do_DELETE = _unsupported
    do_CONNECT = _unsupported
    do_TRACE = _unsupported


def main():
    server = CompanionProxy()
    # shutdown() must run outside the serve_forever thread.
    def stop(_signal, _frame):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    print(json.dumps({"listening": "0.0.0.0:6081", "upstream": "127.0.0.1:60531",
                      "browserOrigin": TRUSTED_ORIGIN}), flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
