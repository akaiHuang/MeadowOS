"""HTTP boundary checks for the Docker bridge, with real loopback requests."""
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import importlib.util
import json
import socket
import tempfile
import threading
import unittest

from companion_proxy import CompanionProxy, PUBLIC_HOST, TRUSTED_ORIGIN


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        payload = self.rfile.read(int(self.headers["Content-Length"]))
        self.server.observed = {"host": self.headers["Host"], "origin": self.headers["Origin"],
                                "body": payload, "client": self.headers.get("X-Meadow-IME-Client")}
        self.send_response(202)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Content-Type", "application/json")
        self.send_header("Access-Control-Allow-Origin", TRUSTED_ORIGIN)
        self.end_headers()
        self.wfile.write(payload)


class ProxyTests(unittest.TestCase):
    def setUp(self):
        self.upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        self.proxy = CompanionProxy(("127.0.0.1", 0), upstream_port=self.upstream.server_port)
        self.threads = [threading.Thread(target=server.serve_forever, daemon=True)
                        for server in (self.upstream, self.proxy)]
        for thread in self.threads:
            thread.start()

    def tearDown(self):
        for server in (self.proxy, self.upstream):
            server.shutdown()
            server.server_close()
        for thread in self.threads:
            thread.join(2)

    def request(self, method="GET", path="/ime/state", body=None, **overrides):
        headers = {"Host": PUBLIC_HOST, "Origin": TRUSTED_ORIGIN, "X-Meadow-IME": "1"}
        headers.update(overrides)
        connection = http.client.HTTPConnection("127.0.0.1", self.proxy.server_port, timeout=2)
        connection.request(method, path, body=body, headers=headers)
        response = connection.getresponse()
        result = response.status, dict(response.getheaders()), response.read()
        connection.close()
        return result

    def test_forward_preserves_original_body_and_client_and_rewrites_host(self):
        body = '{"text":"中文輸入"}'.encode()
        status, headers, payload = self.request("POST", "/ime/preedit", body,
            **{"Content-Type": "application/json", "X-Meadow-IME-Client": "browser-1"})
        self.assertEqual(status, 202)
        self.assertEqual(payload, body)
        self.assertEqual(headers["Access-Control-Allow-Origin"], TRUSTED_ORIGIN)
        self.assertEqual(self.upstream.observed, {"host": f"127.0.0.1:{self.upstream.server_port}",
            "origin": TRUSTED_ORIGIN, "body": body, "client": "browser-1"})

    def test_untrusted_host_origin_unknown_routes_and_methods_are_rejected(self):
        self.assertEqual(self.request(Host="localhost:60531")[0], 403)
        self.assertEqual(self.request(Origin="https://example.test")[0], 403)
        self.assertEqual(self.request(path="/v1/chat/completions")[0], 404)
        self.assertEqual(self.request(path="http://127.0.0.1/ime/state")[0], 400)
        self.assertEqual(self.request("DELETE")[0], 405)

    def raw(self, text):
        with socket.create_connection(("127.0.0.1", self.proxy.server_port), timeout=2) as stream:
            stream.sendall(text.encode())
            return stream.recv(4096).split(b"\r\n", 1)[0]

    def test_duplicate_headers_chunked_body_and_oversize_are_rejected_before_upstream(self):
        prefix = f"POST /attachments/upload HTTP/1.1\r\nHost: {PUBLIC_HOST}\r\nOrigin: {TRUSTED_ORIGIN}\r\n"
        self.assertIn(b"400", self.raw(prefix + "Host: second\r\nContent-Length: 1\r\n\r\nx"))
        self.assertIn(b"400", self.raw(prefix + "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"))
        self.assertIn(b"413", self.raw(prefix + "Content-Length: 20971521\r\n\r\n"))
        self.assertFalse(hasattr(self.upstream, "observed"))

    def test_real_voice_service_accepts_forwarded_origin_and_preflight(self):
        source = Path(__file__).resolve().parents[1] / "packaging/meadowos/voiceos/voice_service.py"
        spec = importlib.util.spec_from_file_location("release_voice_service", source)
        service = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(service)
        self.upstream.shutdown()
        self.upstream.server_close()
        self.threads[0].join(2)
        with tempfile.TemporaryDirectory() as directory:
            Path(directory).chmod(0o700)
            app = service.VoiceApplication(service.VoiceSpool(directory), service.LocalSpeechBackend("/no-model"))
            self.upstream = service.VoiceHTTPServer(("127.0.0.1", 0), app, [TRUSTED_ORIGIN])
            self.proxy.upstream_port = self.upstream.server_port
            thread = threading.Thread(target=self.upstream.serve_forever, daemon=True)
            self.threads[0] = thread
            thread.start()
            status, headers, payload = self.request("GET", "/voice/status", **{"X-Meadow-Voice": "1"})
            self.assertEqual(status, 200)
            self.assertFalse(json.loads(payload)["asr"])
            self.assertEqual(headers["Access-Control-Allow-Origin"], TRUSTED_ORIGIN)
            for route, header in (("/ime/state", "X-Meadow-IME"),
                                  ("/attachments/state", "X-Meadow-Attachments")):
                status, _, payload = self.request("GET", route, **{header: "1"})
                self.assertEqual(status, 200)
                self.assertEqual(json.loads(payload)["version"], 1)
            status, headers, payload = self.request("OPTIONS", "/ime/preedit", **{
                "Access-Control-Request-Method": "POST",
                "Access-Control-Request-Headers": "content-type,x-meadow-ime,x-meadow-ime-client"})
            self.assertEqual(status, 204)
            self.assertEqual(payload, b"")
            self.assertIn("POST", headers["Access-Control-Allow-Methods"])


if __name__ == "__main__":
    unittest.main()
