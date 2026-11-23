import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import time
import unittest

HERE = Path(__file__).resolve().parent
LAUNCHER = runpy.run_path(str(HERE / "agy-enhance"))


class TerminalInputTests(unittest.TestCase):
    def test_split_csi_and_paste(self):
        stream = LAUNCHER["InputStream"]()
        self.assertEqual(list(stream.feed(b"\x1b[101;")), [])
        self.assertEqual(list(stream.feed(b"5u")), [(b"\x1b[101;5u", False)])
        self.assertEqual(list(stream.feed(b"\x1b[200~\x05\x1b[201~\x05")), [
            (b"\x1b[200~", True), (b"\x05", True),
            (b"\x1b[201~", True), (b"\x05", False),
        ])

    def test_subcommand_after_native_option(self):
        self.assertTrue(LAUNCHER["direct_invocation"](["--model", "provider-model", "models"]))
        self.assertTrue(LAUNCHER["direct_invocation"](["--print=hello"]))
        self.assertFalse(LAUNCHER["direct_invocation"](["--model", "models"]))
        self.assertFalse(LAUNCHER["direct_invocation"](["--prompt-interactive", "hello"]))

    def test_settings_roundtrip(self):
        with tempfile.TemporaryDirectory() as directory:
            settings_file = Path(directory) / "config/settings.json"
            settings = {"mode": "local", "model": "example-model", "timeoutSeconds": 10}
            LAUNCHER["save_settings"](settings_file, settings)
            self.assertEqual(LAUNCHER["load_settings"](settings_file), settings)
            self.assertEqual(settings_file.stat().st_mode & 0o777, 0o600)

    def test_paste_started_during_rewrite_stays_blocked_after_completion(self):
        with tempfile.TemporaryDirectory() as directory:
            bridge = LAUNCHER["EditorBridge"](directory)
            running = Path(directory) / "running"
            running.touch()
            with tempfile.TemporaryFile() as child:
                bridge.forward(child.fileno(), b"\x1b[200~first")
                running.unlink()
                bridge.forward(child.fileno(), b"\nsecond\x1b[201~resume")
                child.seek(0)
                self.assertEqual(child.read(), b"resume")


class EditorWorkerTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.directory = Path(temp.name)
        self.draft = self.directory / "draft.txt"
        self.args_log = self.directory / "args.json"
        settings = {"mode": "native", "model": "fixture-model", "timeoutSeconds": 5}
        (self.directory / "settings.json").write_text(json.dumps(settings))
        (self.directory / "disabled-hook.json").write_text('{"enabled": false}')
        binary = self.directory / "native"
        binary.write_text("""#!/usr/bin/env node
const fs = require('node:fs');
fs.writeFileSync(process.env.TEST_ARGS_LOG, JSON.stringify({
  args: process.argv.slice(2), hook: process.env.PROMPT_ENHANCER_CONFIG,
  detached: process.env.ANTIGRAVITY_CONVERSATION_ID === undefined,
}));
setTimeout(() => {
  process.stdout.write(process.env.TEST_REPLY);
  process.exitCode = Number(process.env.TEST_EXIT || 0);
}, Number(process.env.TEST_DELAY || 0));
""")
        binary.chmod(0o700)
        self.env = {
            **os.environ, "AGY_NATIVE_BIN": str(binary), "AGY_ENHANCE_SESSION": str(self.directory),
            "TEST_ARGS_LOG": str(self.args_log), "ANTIGRAVITY_CONVERSATION_ID": "unrelated-session",
        }

    def invoke(self, draft, reply, **kwargs):
        self.draft.write_bytes(draft)
        return subprocess.run(["node", str(HERE / "enhance.mjs"), str(self.draft)],
                              env={**self.env, "TEST_REPLY": reply, **kwargs},
                              capture_output=True, text=True, timeout=8, check=True)

    def test_rewrite_preserves_reference_and_strict_arguments(self):
        self.invoke(b"jelaskan @cache.py tanpa contoh\r\n", "Jelaskan @cache.py tanpa contoh.")
        self.assertEqual(self.draft.read_bytes(), b"Jelaskan @cache.py tanpa contoh.\r\n")
        recorded = json.loads(self.args_log.read_text())
        settings, policy, print_flag, instruction, *rest = recorded["args"]
        self.assertEqual((settings, print_flag), ("--settings", "-p"))
        self.assertEqual(json.loads(policy), json.loads(
            (HERE.parents[1] / "contracts/strict-deny-policy.json").read_text()))
        self.assertIn("<user_untrusted_draft>\njelaskan @cache.py tanpa contoh", instruction)
        self.assertEqual(rest, ["--output-format", "text", "--disable-slash-commands", "--model", "fixture-model"])
        self.assertEqual(recorded["hook"], str(self.directory / "disabled-hook.json"))
        self.assertTrue(recorded["detached"])

    def test_errors_and_unsafe_outputs_leave_bytes_unchanged(self):
        original = b"jelaskan @cache.py"
        for reply, options in [
            ("Jelaskan cache.", {}),
            ("Jelaskan @cache.py.\x07", {}),
            ("Jelaskan @cache.py.", {"TEST_EXIT": "1"}),
        ]:
            with self.subTest(reply=reply, options=options):
                self.invoke(original, reply, **options)
                self.assertEqual(self.draft.read_bytes(), original)

    def test_attachment_refusal_never_starts_provider(self):
        original = b"jelaskan [Image #1]"
        self.invoke(original, "fixture")
        self.assertEqual(self.draft.read_bytes(), original)
        self.assertFalse(self.args_log.exists())

    def test_cancel_preserves_draft(self):
        original = b"jelaskan cache tanpa contoh"
        self.draft.write_bytes(original)
        with subprocess.Popen(["node", str(HERE / "enhance.mjs"), str(self.draft)],
                              env={**self.env, "TEST_REPLY": "Hasil tertunda", "TEST_DELAY": "4000"},
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as worker:
            try:
                deadline = time.monotonic() + 3
                while not self.args_log.exists() and time.monotonic() < deadline:
                    time.sleep(0.025)
                self.assertTrue(self.args_log.exists())
                (self.directory / "cancel").touch()
                self.assertEqual(worker.wait(timeout=3), 0)
            finally:
                if worker.poll() is None:
                    worker.kill()
        self.assertEqual(self.draft.read_bytes(), original)


if __name__ == "__main__":
    unittest.main()
