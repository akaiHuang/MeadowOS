"""Offline onboarding checks. Every credential is an obvious synthetic fixture."""
import contextlib
import http.server
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import threading
import unittest
from unittest import mock
from urllib import request
import warnings

SCRIPT = Path(__file__).with_name("configure_inception.py")
SPEC = importlib.util.spec_from_file_location("configure_inception", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
KEY = "synthetic-fixture-NOT-A-REAL-API-KEY"
LEASE = "a" * 64
RECEIPT = {"applied": True, "connectionId": "inception-fixture", "modelId": "mercury-3",
           "supportsTools": True, "supportsImages": False, "reasoningControl": "inception-effort"}


class FakeClient:
    def __init__(self, fail=None, receipt=None):
        self.calls, self.fail = [], fail
        self.receipt = dict(RECEIPT) if receipt is None else receipt

    def _request(self, method, path, body=None):
        self.calls.append((method, path, body))
        if path == self.fail:
            raise RuntimeError("untrusted remote error echo: " + KEY)
        if method == "POST" and path.endswith("/lease"):
            return {"leaseId": LEASE, "expiresInSeconds": 15}
        if path.endswith("/inception"):
            return self.receipt
        if path == "/agent/model":
            return {"ready": True, "status": {"model": {"id": "mercury-3"}}}
        return {"released": True}


class OnboardingTests(unittest.TestCase):
    def test_native_transaction_order_and_complete_capabilities(self):
        client = FakeClient()
        verifier = mock.Mock()
        result = MODULE.configure(client, KEY, verifier=verifier)
        verifier.assert_called_once_with(KEY)
        self.assertEqual(result, RECEIPT)
        self.assertEqual([(method, path) for method, path, _ in client.calls], [
            ("POST", "/agent/configuration/lease"), ("POST", "/agent/connections/inception"),
            ("GET", "/agent/model"), ("DELETE", "/agent/configuration/lease")])
        self.assertEqual(client.calls[1][2], {"leaseId": LEASE, "apiKey": KEY})

    def test_provider_failure_precedes_any_mutation_or_lease(self):
        client = FakeClient()
        with self.assertRaises(MODULE.SetupError):
            MODULE.configure(client, KEY, verifier=mock.Mock(side_effect=MODULE.SetupError("fixture rejected")))
        self.assertEqual(client.calls, [])

    def test_busy_agent_never_receives_key_and_does_not_kill_work(self):
        client = FakeClient(fail="/agent/configuration/lease")
        with self.assertRaises(MODULE.SetupError) as caught:
            MODULE.configure(client, KEY, verifier=lambda key: None)
        self.assertNotIn(KEY, str(caught.exception))
        self.assertEqual(client.calls, [("POST", "/agent/configuration/lease", None)])

    def test_configuration_failure_releases_lease_and_sanitizes_error(self):
        client = FakeClient(fail="/agent/connections/inception")
        with self.assertRaises(MODULE.SetupError) as caught:
            MODULE.configure(client, KEY, verifier=lambda key: None)
        self.assertNotIn(KEY, str(caught.exception))
        self.assertEqual(client.calls[-1], ("DELETE", "/agent/configuration/lease", {"leaseId": LEASE}))

    def test_ids_only_or_missing_tools_is_not_success(self):
        for receipt in [{"applied": True, "connectionId": "inception-fixture", "modelId": "mercury-3"},
                        dict(RECEIPT, supportsTools=False), dict(RECEIPT, supportsImages=True)]:
            with self.assertRaises(MODULE.SetupError):
                MODULE.configure(FakeClient(receipt=receipt), KEY, verifier=lambda key: None)

    def test_non_tty_and_argument_key_fail_closed_without_echo(self):
        for args in [[], ["--api-key", KEY], [KEY]]:
            result = subprocess.run([sys.executable, str(SCRIPT), *args], input=KEY,
                                    text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn(KEY, result.stdout + result.stderr)

    def test_getpass_echo_fallback_is_an_error(self):
        def insecure_fallback(*args):
            warnings.warn("echo fallback", MODULE.getpass.GetPassWarning)
            return KEY
        with mock.patch.object(MODULE.sys.stdin, "isatty", return_value=True), mock.patch.object(MODULE.getpass, "getpass", side_effect=insecure_fallback):
            with self.assertRaises(MODULE.SetupError):
                MODULE.read_key()


class ProviderHTTPTests(unittest.TestCase):
    def setUp(self):
        fixture = self
        self.calls, self.status, self.payload, self.redirect = [], 200, {"data": [{"id": "mercury-3"}]}, False

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                fixture.calls.append((self.path, self.headers.get("Authorization")))
                data = json.dumps(fixture.payload).encode()
                self.send_response(fixture.status)
                if fixture.redirect:
                    self.send_header("Location", fixture.address + "/credential-sink")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.address = "http://127.0.0.1:" + str(self.server.server_address[1])
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        fixture.opener = request.build_opener(MODULE._NoRedirect())

        class LocalOnlyOpener:
            def open(self, req, timeout):
                if req.full_url != MODULE.MODELS_URL:
                    raise AssertionError("onboarding changed the official endpoint")
                rewritten = request.Request(fixture.address + "/models", headers=dict(req.header_items()))
                return fixture.opener.open(rewritten, timeout=timeout)
        self.local = LocalOnlyOpener()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def test_model_listing_uses_bearer_and_requires_exact_mercury3(self):
        MODULE.verify_provider(KEY, opener=self.local)
        self.assertEqual(self.calls, [("/models", "Bearer " + KEY)])
        self.payload = {"data": [{"id": "mercury-2"}]}
        with self.assertRaises(MODULE.SetupError):
            MODULE.verify_provider(KEY, opener=self.local)

    def test_redirect_is_not_followed_with_key(self):
        self.status, self.redirect = 302, True
        with self.assertRaises(MODULE.SetupError):
            MODULE.verify_provider(KEY, opener=self.local)
        self.assertEqual(len(self.calls), 1)
        self.assertEqual(self.calls[0][0], "/models")

    def test_error_body_cannot_echo_a_key(self):
        self.status, self.payload = 401, {"error": "echoed credential " + KEY}
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            with self.assertRaises(MODULE.SetupError) as caught:
                MODULE.verify_provider(KEY, opener=self.local)
        self.assertNotIn(KEY, str(caught.exception) + stdout.getvalue() + stderr.getvalue())

    def test_invalid_model_payloads_are_rejected(self):
        for payload in [{"data": None}, {"data": [{"id": "mercury-3"}, {"id": "mercury-3"}]},
                        {"data": [{"id": " mercury-3"}]}, {"data": [None]}, {"echo": KEY}]:
            self.payload = payload
            with self.assertRaises(MODULE.SetupError) as caught:
                MODULE.verify_provider(KEY, opener=self.local)
            self.assertNotIn(KEY, str(caught.exception))


if __name__ == "__main__":
    unittest.main()
