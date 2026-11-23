import json
import os
from pathlib import Path
import pty
import runpy
import select
import signal
import subprocess
import tempfile
import termios
import time
import unittest

HERE = Path(__file__).resolve().parent
LAUNCHER = runpy.run_path(str(HERE / "agy-enhance"))


class TerminalInputTests(unittest.TestCase):
    def test_rewrite_holds_composer_and_allows_later_native_editor_switches(self):
        renderer = LAUNCHER["TerminalOutput"]()
        enter = LAUNCHER["ALTERNATE_ON"]
        leave = LAUNCHER["ALTERNATE_OFF"]
        size = (30, 100)
        self.assertEqual(renderer.feed(enter + b"draft", None, size, 1), enter + b"draft")
        self.assertEqual(renderer.feed(leave[:5], 1, size, 2), b"")
        self.assertEqual(renderer.feed(leave[5:], 1, size, 2), LAUNCHER["HIDE_CURSOR"])
        self.assertEqual(renderer.feed(leave + enter[:-1], 1, size, 2), b"")
        self.assertEqual(renderer.feed(enter[-1:] + b"redraw", None, size, 2), b"redraw")
        self.assertEqual(renderer.feed(leave + enter, 1, size, 3), leave + enter)
        self.assertEqual(renderer.finish(), leave + LAUNCHER["SHOW_CURSOR"])
        self.assertEqual(renderer.feed(leave, None, size, 4), leave)
        self.assertEqual(renderer.finish(), b"")

    def test_progress_resizes_and_clears_before_full_redraw(self):
        renderer = LAUNCHER["TerminalOutput"]()
        enter, leave = LAUNCHER["ALTERNATE_ON"], LAUNCHER["ALTERNATE_OFF"]
        size = (30, 90)
        renderer.feed(enter + leave, 1, size, 1)
        phase = "Sedang enhance prompt"
        first = renderer.paint_progress(size, phase, "Alt+K", 1, 3.25)
        self.assertIn(b"\x1b[30;1H", first)
        self.assertIn(b"Sedang enhance prompt (2 dtk)  |  Alt+K untuk batal", first)
        self.assertNotIn(b"\n", first)
        self.assertEqual(renderer.paint_progress(size, phase, "Alt+K", 1, 3.25), b"")
        resized = (32, 19)
        second = renderer.paint_progress(resized, phase, "Alt+K", 1, 4)
        self.assertIn(b"\x1b[30;1H\x1b[2K", second)
        self.assertIn(b"\x1b[32;1H\x1b[2K| Alt+K batal", second)
        self.assertIn(b"\x1b[32;1H\x1b[2K", renderer.feed(enter, None, resized, 4))
        self.assertTrue(renderer.wait_for_composer(False, 4))
        self.assertEqual(renderer.feed(b"\x1b[2", None, resized, 4.1), b"")
        self.assertTrue(renderer.wait_for_composer(False, 4.1))
        self.assertEqual(renderer.feed(b"Jnative", None, resized, 4.2), b"\x1b[2Jnative")
        self.assertFalse(renderer.wait_for_composer(False, 4.2))

    def test_missing_native_repaint_does_not_leave_input_or_cursor_locked(self):
        renderer = LAUNCHER["TerminalOutput"]()
        enter, leave = LAUNCHER["ALTERNATE_ON"], LAUNCHER["ALTERNATE_OFF"]
        renderer.feed(enter + leave, 1, (30, 90), 1)
        self.assertTrue(renderer.wait_for_composer(False, 2))
        self.assertEqual(renderer.release_expired_frame(False, 30, 3.1), LAUNCHER["SHOW_CURSOR"])
        self.assertFalse(renderer.wait_for_composer(False, 3.1))

    def test_input_discarded_during_composer_restore(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryFile() as child:
            bridge = LAUNCHER["EditorBridge"](directory)
            bridge.forward(child.fileno(), b"unreviewed\r\x05", restoring=True)
            self.assertFalse((Path(directory) / "request").exists())
            bridge.forward(child.fileno(), b"safe")
            child.seek(0)
            self.assertEqual(child.read(), b"safe")

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
            self.assertEqual(LAUNCHER["load_settings"](settings_file), {**LAUNCHER["DEFAULTS"], **settings})
            self.assertEqual(settings_file.stat().st_mode & 0o777, 0o600)

    def test_settings_menu_persists_valid_choices(self):
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(
                [str(HERE / "agy-enhance"), "settings", "menu"],
                env={**os.environ, "XDG_CONFIG_HOME": directory},
                input="1\ndetailed\n2\nen\n3\nctrl+c\n3\nalt+k\n0\n",
                capture_output=True, text=True, timeout=5, check=True,
            )
            self.assertIn("dicadangkan", result.stdout)
            settings = json.loads((Path(directory) / "agy-prompt-enhancer/settings.json").read_text())
            self.assertEqual((settings["detail"], settings["language"], settings["shortcut"]),
                             ("detailed", "en", "Alt+K"))

    def test_custom_shortcut_passes_old_key_and_cancels_on_repeat(self):
        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryFile() as child:
            bridge = LAUNCHER["EditorBridge"](directory, "Alt+K")
            bridge.forward(child.fileno(), b"\x05\x1b[107;3u")
            self.assertTrue((Path(directory) / "request").exists())
            bridge.forward(child.fileno(), b"\x03")
            self.assertFalse((Path(directory) / "cancel").exists())
            bridge.forward(child.fileno(), b"\x1bk")
            self.assertTrue((Path(directory) / "cancel").exists())
            child.seek(0)
            self.assertEqual(child.read(), b"\x05\x07")

    def test_session_rendering_preserves_cli_settings_and_shows_active_shortcut(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            args, reserved = LAUNCHER["session_args"](["--settings", '{"mode":"plan"}'], path, "test-status-line")
            self.assertTrue(reserved)
            cli_settings = json.loads(Path(args[1]).read_text())
            self.assertEqual(cli_settings["mode"], "plan")
            self.assertEqual(cli_settings["altScreenMode"], "always")
            self.assertEqual(cli_settings["statusLine"]["command"], "test-status-line")
            source = path / "custom.json"
            custom = {"altScreenMode": "never", "statusLine": {"command": "custom"}}
            source.write_text(json.dumps(custom))
            supplied = [f"--settings={source}"]
            self.assertEqual(LAUNCHER["session_args"](supplied, path, "test-status-line"), (supplied, False))
            (path / "settings.json").write_text('{"shortcut":"Alt+K"}')
            line = LAUNCHER["status_line"](path, {"model": {"display_name": "Gemini", "effort": "high"}})
            self.assertEqual(line, "Enhance Prompt = Alt+K  ·  Gemini  ·  high")
            (path / "outcome").write_text("applied")
            self.assertEqual(LAUNCHER["status_line"](path, {}), "Enhance Prompt = Alt+K")

    def test_final_footer_expires_without_input_or_native_redraw(self):
        for result, expected in (("applied", "Enhance selesai"), ("cancelled", "Enhance dibatalkan")):
            with self.subTest(result=result), tempfile.TemporaryDirectory() as directory:
                bridge = LAUNCHER["EditorBridge"](directory)
                bridge.requested = 1
                (Path(directory) / "outcome").write_text(result)
                renderer = LAUNCHER["TerminalOutput"]()
                size = (24, 80)
                enter = LAUNCHER["ALTERNATE_ON"]
                renderer.feed(enter, None, size, 1)
                self.assertIn(expected.encode(), renderer.paint_footer(size, bridge.final_status(10)))
                self.assertEqual(bridge.final_status(16.99), expected)
                self.assertIsNone(bridge.final_status(17))
                self.assertEqual(renderer.paint_footer(size, bridge.final_status(17)), b"\x1b7\x1b[24;1H\x1b[2K\x1b8")
                self.assertIsNone(bridge.final_status(18))

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


class TerminalSessionTests(unittest.TestCase):
    def test_progress_and_input_gate_through_pty_roundtrip(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            native = home / "agy-fixture"
            native.write_text(r"""#!/usr/bin/env python3
import os, shlex, subprocess, sys, time, tty
from pathlib import Path

home = Path(os.environ["AGY_TEST_DIRECTORY"])
if "-p" in sys.argv:
    while not (home / "release").exists():
        time.sleep(0.02)
    print("Jelaskan cache.")
else:
    rows = os.get_terminal_size().lines
    tty.setraw(0)
    os.write(1, f"\x1b[?1049hDRAFT_ORIGINAL native-rows={rows}\r\n".encode())
    while data := os.read(0, 65536):
        if data == b"\x07":
            draft = home / "draft.txt"
            draft.write_text("jelaskan cache")
            os.write(1, b"\x1b[?1049l")
            subprocess.run([*shlex.split(os.environ["EDITOR"]), str(draft)], check=True)
            os.write(1, b"\x1b[?1049hPENDING_REPAINT")
            time.sleep(0.4)
            os.write(1, b"\x1b[H\x1b[2J" + draft.read_bytes())
        else:
            os.write(1, b"NATIVE_INPUT:" + data)
""")
            native.chmod(0o700)
            pid, fd = pty.fork()
            if pid == 0:
                try:
                    termios.tcsetwinsize(0, (24, 80))
                    env = {**os.environ, "AGY_NATIVE_BIN": str(native),
                           "AGY_TEST_DIRECTORY": directory, "XDG_CONFIG_HOME": directory}
                    os.execve(str(HERE / "agy-enhance"), ["agy-enhance"], env)
                finally:
                    os._exit(127)

            output = bytearray()

            def read_until(needle):
                deadline = time.monotonic() + 5
                while needle not in output and time.monotonic() < deadline:
                    if select.select([fd], [], [], 0.05)[0]:
                        output.extend(os.read(fd, 65536))
                self.assertIn(needle, output)

            try:
                read_until(b"DRAFT_ORIGINAL native-rows=23")
                os.write(fd, b"\x05")
                read_until(b"Sedang enhance prompt")
                self.assertIn(b"\x1b[24;1H", output)
                self.assertNotIn(b"\x1b[?1049l", output)
                os.write(fd, b"WHILE_WORKING\r")
                (home / "release").touch()
                read_until(b"PENDING_REPAINT")
                os.write(fd, b"DURING_REPAINT\r")
                read_until(b"\x1b[2JJelaskan cache.")
                read_until(b"Enhance selesai")
                os.write(fd, b"AFTER_REPAINT")
                read_until(b"NATIVE_INPUT:AFTER_REPAINT")
                self.assertNotIn(b"WHILE_WORKING", output)
                self.assertNotIn(b"DURING_REPAINT", output)
                self.assertEqual((home / "draft.txt").read_text(), "Jelaskan cache.")
            finally:
                try:
                    os.kill(pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                os.close(fd)
                os.waitpid(pid, 0)


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
        self.assertEqual((self.directory / "outcome").read_text(), "applied")
        recorded = json.loads(self.args_log.read_text())
        settings, policy, print_flag, instruction, *rest = recorded["args"]
        self.assertEqual((settings, print_flag), ("--settings", "-p"))
        self.assertEqual(json.loads(policy), json.loads(
            (HERE.parents[1] / "contracts/strict-deny-policy.json").read_text()))
        self.assertIn("<user_untrusted_draft>\njelaskan @cache.py tanpa contoh", instruction)
        self.assertEqual(rest, ["--output-format", "text", "--disable-slash-commands", "--model", "fixture-model"])
        self.assertEqual(recorded["hook"], str(self.directory / "disabled-hook.json"))
        self.assertTrue(recorded["detached"])

    def test_forced_language_and_detail_are_sent_only_as_rewrite_preferences(self):
        settings = self.directory / "settings.json"
        base = json.loads(settings.read_text())
        for language, detail, expected_lang, expected_detail in [
            ("en", "detailed", "English", "existing goals, constraints, and deliverables explicit"),
            ("id", "concise", "Bahasa Indonesia", "fewest words"),
        ]:
            with self.subTest(language=language, detail=detail):
                settings.write_text(json.dumps({**base, "language": language, "detail": detail, "shortcut": "Alt+K"}))
                result = self.invoke(b"jelaskan cache tanpa contoh", "Explain caching without examples.")
                instruction = json.loads(self.args_log.read_text())["args"][3]
                policy = instruction.split("<user_untrusted_draft>\n")[0]
                self.assertIn(f"Write the rewritten instruction in {expected_lang}", policy)
                self.assertIn(expected_detail, policy)
                self.assertNotIn("Maintain the user's original language exactly", policy)
                self.assertIn("Tekan Alt+K lagi untuk membatalkan", result.stderr)

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
                self.assertEqual((self.directory / "outcome").read_text(), "failed")

    def test_attachment_refusal_never_starts_provider(self):
        original = b"jelaskan [Image #1]"
        self.invoke(original, "fixture")
        self.assertEqual(self.draft.read_bytes(), original)
        self.assertFalse(self.args_log.exists())
        self.assertEqual((self.directory / "outcome").read_text(), "protected")

    def test_timeout_preserves_draft_and_reports_outcome(self):
        config = self.directory / "settings.json"
        settings = json.loads(config.read_text())
        config.write_text(json.dumps({**settings, "timeoutSeconds": 0.15}))
        original = b"jelaskan cache tanpa contoh"
        self.invoke(original, "Hasil terlambat.", TEST_DELAY="4000")
        self.assertEqual(self.draft.read_bytes(), original)
        self.assertEqual((self.directory / "outcome").read_text(), "timeout")

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
        self.assertEqual((self.directory / "outcome").read_text(), "cancelled")


if __name__ == "__main__":
    unittest.main()
