import argparse
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("stability_check", Path(__file__).with_name("stability-check.py"))
assert SPEC is not None and SPEC.loader is not None
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class StabilityTests(unittest.TestCase):
    def test_default_and_help_do_not_spawn_or_write(self) -> None:
        with mock.patch.object(RUNNER.subprocess, "Popen", side_effect=AssertionError("spawn")), \
             mock.patch.object(RUNNER.Path, "mkdir", side_effect=AssertionError("mkdir")), \
             mock.patch.object(RUNNER, "execute", side_effect=AssertionError("execute")), \
             contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(RUNNER.main([]), 0)
            with self.assertRaises(SystemExit) as result:
                RUNNER.main(["--help"])
            self.assertEqual(result.exception.code, 0)

    def test_duration_and_run_admission(self) -> None:
        self.assertEqual(RUNNER.seconds("8h"), 28800)
        for value in ("0h", "8.0h", "$(command)", "../1m"):
            with self.assertRaises(argparse.ArgumentTypeError):
                RUNNER.seconds(value)
        for args in (["--duration", "9h"], ["--duration", "30s"], ["--interval", "61s"], ["--run"]):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                RUNNER.main(args)

    def test_final_evidence_is_strict(self) -> None:
        process = mock.Mock(returncode=0)
        lane = RUNNER.Lane("recovery", process, "1")
        lane.report = {"schema": "stability-soak/v1", "mode": "recovery", "state": "passed", "requestedMs": 60000,
                       "elapsedMs": 60001, "workersJoined": True, "noReplay": True, "resourcesWithinBudget": True,
                       "path": "relay", "payloadHashes": RUNNER.HASHES.copy(), "bytesEachWay": 9 << 20,
                       "faults": {"cancel": 1, "transport": 1, "device": 1}}
        self.assertTrue(RUNNER.passed(lane, 60))
        for key, value in (("state", "running"), ("elapsedMs", 59999), ("workersJoined", False), ("noReplay", False),
                           ("resourcesWithinBudget", False), ("path", "unknown"), ("payloadHashes", {}), ("faults", {})):
            with self.subTest(key=key):
                old = lane.report[key]
                lane.report[key] = value
                self.assertFalse(RUNNER.passed(lane, 60))
                lane.report[key] = old
        process.returncode = 1
        self.assertFalse(RUNNER.passed(lane, 60))

    def test_reaped_process_is_never_signaled(self) -> None:
        process = mock.Mock()
        process.poll.return_value = 0
        lane = RUNNER.Lane("steady", process, "1")
        with mock.patch.object(RUNNER.os, "killpg", side_effect=AssertionError("signal")):
            lane.stop()

    def test_pid_identity_mismatch_is_never_signaled(self) -> None:
        process = mock.Mock(pid=123)
        process.poll.return_value = None
        lane = RUNNER.Lane("steady", process, "1")
        with mock.patch.object(RUNNER, "proc_start", return_value="other"), \
             mock.patch.object(RUNNER.os, "killpg", side_effect=AssertionError("signal")):
            with self.assertRaisesRegex(ValueError, "process_identity_changed"):
                lane.stop()

    def test_report_budget_and_atomic_round_trip(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "status.json"
            self.assertIsNone(RUNNER.read_report(path))
            RUNNER.atomic_json(path, {"schema": "stability-soak/v1", "state": "running"})
            self.assertEqual(RUNNER.read_report(path)["state"], "running")
            self.assertEqual(json.loads(path.read_text())["schema"], "stability-soak/v1")
            path.write_bytes(b" " * (RUNNER.REPORT_LIMIT + 1))
            with self.assertRaises(ValueError):
                RUNNER.read_report(path)


if __name__ == "__main__":
    unittest.main()
