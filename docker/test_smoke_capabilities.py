"""Known WebKit container restrictions must not swallow unknown app failures."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("meadow_smoke", Path(__file__).with_name("smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class CapabilityClassificationTests(unittest.TestCase):
    def test_exact_observed_namespace_and_proxy_failure_is_recognized(self):
        log = ("bwrap: No permissions to create a new namespace, likely because the kernel does not allow non-privileged user namespaces.\n"
               "ERROR: Failed to fully launch dbus-proxy: Child process exited with code 1\n")
        self.assertTrue(smoke.known_namespace_restriction(log))

    def test_namespace_message_alone_is_not_enough(self):
        self.assertFalse(smoke.known_namespace_restriction("bwrap: No permissions to create a new namespace"))

    def test_unrelated_proxy_or_browser_failures_are_not_masked(self):
        for log in ("Failed to fully launch dbus-proxy: Child process exited with code 1",
                    "Cannot use --private-instance and --profile at the same time",
                    "Segmentation fault", "", "bwrap: Can't mount proc on /newroot/proc: Operation not permitted"):
            with self.subTest(log=log):
                self.assertFalse(smoke.known_namespace_restriction(log))


if __name__ == "__main__":
    unittest.main()
