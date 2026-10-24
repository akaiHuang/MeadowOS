"""Explicit MeadowOS launcher policy; fixtures never execute an Agent or tool."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "meadow_full_access_runtime",
    Path(__file__).resolve().parents[2] / "packaging/meadowos/runtime.py",
)
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class FullAccessLaunchTests(unittest.TestCase):
    def test_os_policy_is_explicit_and_does_not_mutate_calling_environment(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(
            os.environ, {"HOME": directory, "MEADOW_APPROVAL_MODE": "default"}, clear=True
        ):
            env = runtime.runtime_environment()
            self.assertEqual(env["MEADOW_APPROVAL_MODE"], "full-access")
            self.assertEqual(env["MEADOW_CONVERSATION_PROFILE"], "meadow-os")
            self.assertEqual(os.environ["MEADOW_APPROVAL_MODE"], "default")

    def test_agent_and_managed_desktop_share_the_explicit_os_policy(self):
        for component in ["agent", "desktop"]:
            with self.subTest(component=component), tempfile.TemporaryDirectory() as directory:
                home = Path(directory)
                workspace = home / "MeadowWork"
                prefix = home / "opt/meadow"
                with patch.dict(os.environ, {"HOME": directory}, clear=True), \
                     patch.object(runtime, "executable"), \
                     patch.object(runtime, "initialize_state", return_value=(home, home, workspace)), \
                     patch.object(runtime, "require_x11"), \
                     patch.object(runtime.os, "chdir"), patch.object(runtime.os, "execve") as execute:
                    runtime.run_component(prefix, component)
                    execute.assert_called_once()
                    executable, command, env = execute.call_args.args
                    self.assertEqual(env["MEADOW_APPROVAL_MODE"], "full-access")
                    self.assertEqual(env["MEADOW_WORKSPACE"], str(workspace))
                    if component == "agent":
                        self.assertEqual(executable, str(prefix / "libexec/meadow-agentd"))
                        self.assertEqual(command[command.index("--approval-mode") + 1], "full-access")
                    else:
                        self.assertEqual(command, [str(prefix / "bin/meadow-desktop")])
                        self.assertEqual(env["MEADOW_AGENTD_MANAGED"], "1")


if __name__ == "__main__":
    unittest.main()
