"""Pure logic and mocked lifecycle checks; never creates a network fixture."""
from __future__ import annotations

import argparse
import base64
import shlex
import stat
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import signal
import sys
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location("lab_fixture", Path(__file__).with_name("lab-fixture.py"))
assert SPEC is not None and SPEC.loader is not None
fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(fixture)
REAL_SYSTEM_COMMAND = fixture.system_command
REAL_TRUSTED_TOOL = fixture.trusted_tool


class ProtocolStream:
    def __init__(self, data: bytes = b"") -> None:
        self.buffer = io.BytesIO(data)

    def write(self, value: str) -> int:
        return self.buffer.write(value.encode())

    def flush(self) -> None:
        pass


def protocol(command: str, payload: bytes = b"") -> tuple[int, bytes]:
    output = ProtocolStream()
    with patch.object(sys, "stdin", ProtocolStream(payload)), patch.object(sys, "stdout", output), patch.dict(
        fixture.os.environ, {"SSH_ORIGINAL_COMMAND": command}
    ), patch.object(signal, "alarm") as alarm:
        code = 0
        try:
            exec(compile(fixture.FORCE_COMMAND, "<fixture-protocol>", "exec"), {})
        except SystemExit as exc:
            code = int(exc.code)
        alarm.assert_called_once_with(30)
    return code, output.buffer.getvalue()


class FixtureTests(unittest.TestCase):
    def setUp(self) -> None:
        # Runtime launches stay mocked; trust-policy tests below call real pure helpers explicitly.
        self.addCleanup(patch.stopall)
        patch.object(fixture, "system_command", side_effect=lambda argv: list(argv)).start()
        patch.object(fixture, "trusted_tool", side_effect=lambda name: "/usr/bin/" + name).start()

    def test_default_and_help_never_touch_runtime(self) -> None:
        for arguments in ([], ["--help"], ["--child"], ["--output", "/unused"]):
            with self.subTest(arguments=arguments), patch.object(sys, "argv", ["fixture", *arguments]), patch.object(
                fixture, "parent", side_effect=AssertionError("runtime")
            ), patch.object(fixture, "child", side_effect=AssertionError("runtime")), patch.object(
                fixture.os, "umask", side_effect=AssertionError("mutation")
            ), contextlib.redirect_stdout(io.StringIO()):
                try:
                    self.assertEqual(fixture.main(), 0)
                except SystemExit as exc:
                    self.assertEqual(exc.code, 0)

    def test_run_requires_explicit_actor_and_paths(self) -> None:
        with patch.object(sys, "argv", ["fixture", "--run"]), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as raised:
                fixture.main()
            self.assertEqual(raised.exception.code, 2)

    def test_network_overlap_checks_all_tables_and_addresses(self) -> None:
        self.assertEqual(fixture.conflicting_networks([], [{"dst": "default"}, {"dst": "::/0"}]), [])
        for network in ("169.254.0.0/16", "169.254.91.3/32", "128.0.0.0/1"):
            self.assertTrue(fixture.conflicting_networks([], [{"dst": network, "table": 99}]))
        self.assertTrue(fixture.conflicting_networks(
            [{"addr_info": [{"family": "inet", "local": "169.254.91.6", "prefixlen": 32}]}], []))
        self.assertEqual(fixture.conflicting_networks([], [{"dst": "169.254.92.0/24"}]), [])

    def test_fixed_marker_and_reject_unknown(self) -> None:
        for command in ("", "read-only-marker"):
            self.assertEqual(protocol(command), (0, b"remote-enet-mgu-fixture"))
        for command in ("id", "read-only-marker x", "read-only-bulk 1", "read-only-upload", "read-only-bulk; id"):
            self.assertEqual(protocol(command), (2, b""))

    def test_bulk_and_upload_both_sizes(self) -> None:
        for size in (1048576, 8388608):
            with self.subTest(size=size):
                data = bytes(size)
                self.assertEqual(protocol(f"read-only-bulk {size}"), (0, data))
                code, receipt = protocol(f"read-only-upload {size}", data)
                self.assertEqual(code, 0)
                self.assertEqual(json.loads(receipt), {"bytes": size, "sha256": hashlib.sha256(data).hexdigest()})
        self.assertEqual(protocol("read-only-bulk"), (0, bytes(1048576)))

    def test_upload_rejects_short_excess_and_nonzero(self) -> None:
        for data in (b"", bytes(1048575), bytes(1048577), b"x" + bytes(1048575)):
            self.assertEqual(protocol("read-only-upload 1048576", data), (2, b""))

    def test_identification_is_full_56_bytes(self) -> None:
        packet = fixture.identification()
        self.assertEqual(len(packet), 56)
        self.assertEqual(packet[:6], bytes.fromhex("000000320011"))

    def test_discovery_only_responds_to_fixed_source_and_exact_request(self) -> None:
        class Listener:
            def __init__(self) -> None:
                self.responses = []
                self.inputs = iter([
                    (bytes.fromhex("000000000011"), ("169.254.91.4", 123)),
                    (bytes.fromhex("000000000011") + b"x", ("169.254.91.1", 123)),
                    (bytes.fromhex("000000000011"), ("169.254.91.1", 123)),
                ])

            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def settimeout(self, timeout: float) -> None:
                pass

            def recvfrom(self, limit: int):
                return next(self.inputs)

            def sendmsg(self, payload, ancillary, flags, peer):
                self.responses.append((payload, ancillary, peer))
                return 56

        listener = Listener()
        with patch.object(fixture.socket, "socket", return_value=listener), patch.object(
            sys, "argv", ["worker", "9", "1"]
        ), patch.object(signal, "alarm"), patch.object(fixture.time, "monotonic", side_effect=[0, 0, 0, 0, 2]):
            exec(compile(fixture.DISCOVERY_PROGRAM, "<discovery>", "exec"), {})
        self.assertEqual(len(listener.responses), 1)
        payload, ancillary, peer = listener.responses[0]
        self.assertEqual(payload, [fixture.identification()])
        self.assertEqual(peer, ("169.254.91.1", 123))
        self.assertEqual(ancillary[0][2][4:8], fixture.socket.inet_aton("169.254.91.2"))

    def test_cleanup_requires_matching_interface_alias(self) -> None:
        self.assertFalse(fixture.owned_link([], "private", "nonce"))
        self.assertTrue(fixture.owned_link([{"ifname": "private", "ifalias": "nonce"}], "private", "nonce"))
        for entry in ({"ifname": "private"}, {"ifname": "private", "ifalias": "other"}):
            with self.assertRaises(RuntimeError):
                fixture.owned_link([entry], "private", "nonce")

    def test_process_cleanup_reports_normal_signal_and_force(self) -> None:
        for result, forced in ((0, False), (-15, False), (-9, True)):
            process = Mock(pid=123, returncode=result)
            process.poll.return_value = None
            if forced:
                process.wait.side_effect = [fixture.subprocess.TimeoutExpired("mock", 5), result]
            with patch.object(fixture.os, "killpg") as killpg:
                report = fixture.stop_process(process, group=True)
            self.assertEqual(report["forced"], forced)
            self.assertEqual(report["exit_kind"], "normal" if result == 0 else "signal")
            self.assertEqual(killpg.call_count, 2 if forced else 1)

    def test_exited_group_leader_does_not_signal_reused_pid(self) -> None:
        process = Mock(pid=123, returncode=0)
        process.poll.return_value = 0
        with patch.object(fixture.os, "killpg") as killpg:
            fixture.stop_process(process, group=True)
        killpg.assert_not_called()

    def test_export_is_explicit_relative_path_whitelist(self) -> None:
        value = fixture.export_metadata("ordinary_actor", "ssh-ed25519 TEST fixture\n")
        self.assertEqual(set(value), {"schema_version", "gateway", "target", "ssh_user",
                                     "identity_file", "host_key_file", "host_public_key"})
        self.assertEqual(value["ssh_user"], "ordinary_actor")
        self.assertEqual(value["host_public_key"], "ssh-ed25519 TEST fixture")
        for field in ("identity_file", "host_key_file"):
            self.assertFalse(Path(value[field]).is_absolute())
            self.assertNotIn("..", Path(value[field]).parts)

    def test_command_output_limit_kills_and_waits(self) -> None:
        process = Mock()
        process.__enter__ = Mock(return_value=process)
        process.__exit__ = Mock(return_value=False)
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=False)
        selector.get_map.return_value = True
        selector.select.return_value = [(Mock(fd=9), 1)]
        with patch.object(fixture.subprocess, "Popen", return_value=process), patch.object(
            fixture.selectors, "DefaultSelector", return_value=selector
        ), patch.object(fixture.os, "read", return_value=bytes(1048577)):
            with self.assertRaisesRegex(RuntimeError, "output limit"):
                fixture.run(["mock-only"])
        process.kill.assert_called_once()
        process.wait.assert_called_once_with(timeout=5)

    def test_namespace_reclaimer_never_signals_unowned_namespace(self) -> None:
        with patch.object(Path, "iterdir", return_value=iter([Path("/proc/12")])), patch.object(
            fixture.os, "pidfd_open", return_value=99
        ), patch.object(fixture.os, "readlink", return_value="net:[unrelated]"), patch.object(
            fixture.signal, "pidfd_send_signal"
        ) as send, patch.object(fixture.os, "close") as close:
            self.assertEqual(fixture.reclaim_namespace_processes("net:[fixture]"), [])
        send.assert_not_called()
        close.assert_called_once_with(99)

    def test_network_failure_at_every_command_keeps_unmarked_pair_off_host(self) -> None:
        # Model failures both before and after command completion (timeout ambiguity).
        for fail_at in range(1, 14):
            for fail_after in (False, True):
                state: dict[str, dict[str, str]] = {}
                calls: list[list[str]] = []
                report = {}

                def entries(namespace: str) -> list[dict[str, str]]:
                    return [{"ifname": name, "ifalias": item["alias"]}
                            for name, item in state.items() if item["namespace"] == namespace]

                def command(argv: list[str]) -> str:
                    calls.append(argv)
                    failing = len(calls) == fail_at
                    if failing and not fail_after:
                        raise RuntimeError("injected before command")
                    child_side = argv[0] == "nsenter"
                    tail = argv[argv.index("ip") + 1:] if child_side else argv[1:]
                    result = ""
                    if tail[:3] == ["-j", "-d", "link"]:
                        result = fixture.json.dumps([{"ifname": "lo"}] + entries("child"))
                    elif tail[:2] == ["link", "add"]:
                        self.assertTrue(child_side)
                        state.update({name: {"namespace": "child", "alias": ""} for name in ("host", "peer")})
                    elif "alias" in tail:
                        state[tail[tail.index("dev") + 1]]["alias"] = tail[tail.index("alias") + 1]
                    elif "netns" in tail:
                        self.assertEqual(state["host"]["alias"], "nonce")
                        self.assertEqual(state["peer"]["alias"], "nonce")
                        state["host"]["namespace"] = "parent"
                    if failing and fail_after:
                        raise RuntimeError("injected after command")
                    return result

                process = Mock(pid=23)
                process.poll.return_value = None
                def readlink(path: str) -> str:
                    return "net:[1]" if path == "/proc/self/ns/net" else "net:[2]"
                with self.subTest(fail_at=fail_at, after=fail_after), patch.object(fixture, "STOP", False), patch.object(
                    fixture, "run", side_effect=command
                ), patch.object(fixture, "links", side_effect=lambda: entries("parent")), patch.object(
                    fixture, "snapshot_conflicts", return_value=[]
                ), patch.object(fixture.os, "readlink", side_effect=readlink), patch.object(
                    fixture.os, "fstat", return_value=Mock(st_ino=2)
                ):
                    if fail_at <= 12:
                        with self.assertRaises(RuntimeError):
                            fixture.configure_network(process, 99, "net:[2]", "net:[1]", "host", "peer", "nonce", report)
                    else:
                        fixture.configure_network(process, 99, "net:[2]", "net:[1]", "host", "peer", "nonce", report)
                        self.assertEqual(len(calls), 12)
                for entry in entries("parent"):
                    self.assertEqual(entry["ifalias"], "nonce")
                    self.assertTrue(report["host_move_attempted"])
                completed = [step for step in report.get("network_steps", []) if step["completed"]]
                self.assertLessEqual(len(completed), 10)

    def test_network_alias_write_silently_ignored_never_moves_host(self) -> None:
        process = Mock(pid=23)
        process.poll.return_value = None
        outputs = [json.dumps([{"ifname": "lo"}]), "", "", "",
                   json.dumps([{"ifname": "host"}, {"ifname": "peer"}])]
        report = {}
        with patch.object(fixture, "STOP", False), patch.object(fixture, "run", side_effect=outputs) as run, patch.object(
            fixture, "links", return_value=[]
        ), patch.object(fixture, "snapshot_conflicts", return_value=[]), patch.object(
            fixture.os, "readlink", side_effect=lambda path: "net:[1]" if path == "/proc/self/ns/net" else "net:[2]"
        ), patch.object(fixture.os, "fstat", return_value=Mock(st_ino=2)):
            with self.assertRaisesRegex(RuntimeError, "ownership"):
                fixture.configure_network(process, 99, "net:[2]", "net:[1]", "host", "peer", "nonce", report)
        self.assertFalse(report.get("host_move_attempted", False))
        self.assertTrue(all("netns" not in call.args[0] for call in run.call_args_list))

    def test_child_pre_discovery_cancellation_is_not_cleanup_failure(self) -> None:
        for failure, expected in ((fixture.Cancelled("cancelled"), 0), (RuntimeError("startup failed"), 1)):
            args = argparse.Namespace(parent_netns="net:[1]", token="private", output="/not-used", lifetime_seconds=30)
            written = {}
            with patch.object(fixture.os, "geteuid", return_value=0), patch.object(
                fixture.os, "readlink", return_value="net:[2]"
            ), patch.object(fixture, "links", return_value=[{"ifname": "lo"}]), patch.object(
                fixture, "wait_file", side_effect=failure
            ), patch.object(fixture, "write_json", side_effect=lambda path, value: written.update({path.name: value})), patch.object(
                fixture, "debug_error"
            ):
                self.assertEqual(fixture.child(args), expected)
            report = written["child-report.json"]
            self.assertFalse(report.get("cleanup_errors"))
            self.assertEqual(report["discovery"]["state"], "not-started")
            self.assertIsNone(report["discovery"]["joined"])
            self.assertEqual("error" in report, expected == 1)

    def test_child_partial_startup_distinguishes_reaped_from_cleanup_failure(self) -> None:
        for reap_failed in (False, True):
            args = argparse.Namespace(parent_netns="net:[1]", token="private", output="/not-used", lifetime_seconds=30,
                                      actor_uid=123, actor_gid=456, fakezgw_bin="/fake", actor_directory="/actor")
            discovery = Mock(pid=42)
            discovery.poll.return_value = None if reap_failed else 0
            listener = Mock()
            listener.getsockname.return_value = ("0.0.0.0", 6811)
            listener.fileno.return_value = 99
            written = {}
            with patch.object(fixture.os, "geteuid", return_value=0), patch.object(
                fixture.os, "readlink", return_value="net:[2]"
            ), patch.object(fixture, "links", return_value=[{"ifname": "lo"}]), patch.object(
                fixture, "wait_file", return_value={"token": "private"}
            ), patch.object(fixture.socket, "socket", return_value=listener), patch.object(
                fixture.subprocess, "Popen", return_value=discovery
            ), patch.object(fixture, "start_service", side_effect=RuntimeError("fake startup failed")), patch.object(
                fixture, "stop_process", side_effect=RuntimeError("reap failed") if reap_failed else None,
                return_value={"pid": 42, "returncode": 0, "forced": False, "exit_kind": "normal"}
            ), patch.object(fixture, "write_json", side_effect=lambda path, value: written.update({path.name: value})), patch.object(
                fixture, "debug_error"
            ):
                self.assertEqual(fixture.child(args), 1)
            report = written["child-report.json"]
            self.assertEqual(report["error"], "startup-failed")
            self.assertEqual(bool(report.get("cleanup_errors")), reap_failed)
            self.assertEqual(report["discovery"]["state"], "reap-failed" if reap_failed else "reaped")
            self.assertEqual(report["discovery"]["joined"], not reap_failed)

    def test_parent_namespace_change_prevents_any_network_command(self) -> None:
        process = Mock(pid=23)
        process.poll.return_value = None
        with patch.object(fixture, "STOP", False), patch.object(fixture.os, "readlink", return_value="net:[wrong]"), patch.object(
            fixture, "run"
        ) as run:
            with self.assertRaisesRegex(RuntimeError, "parent namespace"):
                fixture.configure_network(process, 99, "net:[2]", "net:[1]", "host", "peer", "nonce", {})
        run.assert_not_called()

    def test_private_debug_is_bounded_redacted_and_cannot_break_cleanup(self) -> None:
        with patch.object(fixture.os, "open", return_value=99) as opened, patch.object(
            fixture.os, "fstat", return_value=Mock(st_size=0)
        ), patch.object(fixture.os, "write") as write, patch.object(fixture.os, "close"):
            fixture.debug_error(Path("/not-used"), "create-child-veth", RuntimeError("failure " + "x" * 5000))
            event = json.loads(write.call_args.args[1])
            self.assertEqual(event["category"], "RuntimeError")
            self.assertEqual(len(event["message"]), 2048)
            self.assertEqual(opened.call_args.args[2], 0o600)
            fixture.debug_error(Path("/not-used"), "prepare", RuntimeError("PRIVATE KEY hidden"))
            self.assertNotIn("hidden", write.call_args.args[1].decode())
        with patch.object(fixture.os, "open", side_effect=PermissionError):
            fixture.debug_error(Path("/not-used"), "cleanup", RuntimeError("failure"))
        with patch.object(fixture.os, "open", return_value=99), patch.object(
            fixture.os, "fstat", return_value=Mock(st_size=65536)
        ), patch.object(fixture.os, "write") as write, patch.object(fixture.os, "close"):
            fixture.debug_error(Path("/not-used"), "cleanup", RuntimeError("failure"))
        write.assert_not_called()

    def test_sshd_authorization_only_pins_this_run_public_key(self) -> None:
        raw = fixture.struct.pack("!I", 11) + b"ssh-ed25519" + fixture.struct.pack("!I", 32) + bytes(range(32))
        encoded = base64.b64encode(raw).decode("ascii")
        config = fixture.sshd_config(Path("/tmp/private-root"), "ordinary_actor", "ssh-ed25519 " + encoded + " ignored-comment")
        lines = config.splitlines()
        self.assertIn("AuthorizedKeysFile none", lines)
        command = next(line for line in lines if line.startswith("AuthorizedKeysCommand "))
        self.assertEqual(shlex.split(command), ["AuthorizedKeysCommand", "/usr/bin/echo", "ssh-ed25519", encoded])
        self.assertIn("AuthorizedKeysCommandUser ordinary_actor", lines)
        self.assertIn("UsePAM no", lines)
        self.assertIn("StrictModes yes", lines)
        self.assertEqual(config.count(encoded), 1)
        self.assertNotIn("ignored-comment", config)
        self.assertNotIn("userkey.pub", config)
        self.assertNotIn("authorized_keys", config)
        self.assertNotIn("%", command)
        other_raw = raw[:-32] + bytes(32)
        other_key = base64.b64encode(other_raw).decode("ascii")
        self.assertNotIn(other_key, config)
        for invalid in ("ssh-rsa " + encoded, "ssh-ed25519 %u", "ssh-ed25519 AAAA", "ssh-ed25519 " + encoded + ";id"):
            with self.assertRaises(ValueError):
                fixture.sshd_config(Path("/tmp/private-root"), "ordinary_actor", invalid)

    def test_authorization_command_requires_root_owned_nonwritable_path(self) -> None:
        executable = Mock(st_uid=0, st_mode=stat.S_IFREG | 0o755)
        with patch.object(Path, "resolve", return_value=Path("/usr/bin/echo")), patch.object(Path, "stat", return_value=executable):
            fixture.validate_authorization_command()
        for uid, mode in ((1000, stat.S_IFREG | 0o755), (0, stat.S_IFREG | 0o775),
                          (0, stat.S_IFREG | 0o757), (0, stat.S_IFREG | 0o644), (0, stat.S_IFDIR | 0o755)):
            with self.subTest(uid=uid, mode=mode), patch.object(Path, "resolve", return_value=Path("/usr/bin/echo")), patch.object(
                Path, "stat", return_value=Mock(st_uid=uid, st_mode=mode)
            ):
                with self.assertRaises(RuntimeError):
                    fixture.validate_authorization_command()

    def probe_report(self, size: int = 1048576) -> dict:
        digest = hashlib.sha256(bytes(size)).hexdigest()
        phases = []
        for stage in ("keys", "discover", "ssh", "marker", "download", "upload"):
            phase = {"stage": stage, "code": "ok", "duration_ms": 1, "bytes": 0}
            if stage in ("download", "upload"):
                phase.update(bytes=size, sha256=digest)
            phases.append(phase)
        return {"schema": "labprobe/v1", "version": "dev", "platform": "linux/amd64", "success": True,
                "dry_run": False, "cancelled": False, "timeout": False, "cleanup": True,
                "expected_bytes": size, "expected_sha256": digest, "phases": phases}

    def test_probe_report_requires_complete_exact_success(self) -> None:
        for size in (1048576, 8388608):
            self.assertTrue(fixture.validate_probe_report(self.probe_report(size), size))
        changes = [lambda v: v.pop("schema"), lambda v: v.update(cleanup=False),
                   lambda v: v.update(cancelled=True), lambda v: v.update(timeout=True),
                   lambda v: v.update(dry_run=True), lambda v: v.update(success=1),
                   lambda v: v["phases"].pop(3), lambda v: v["phases"][3].update(code="failed"),
                   lambda v: v["phases"][4].update(sha256="0" * 64),
                   lambda v: v["phases"][5].update(bytes=1048575),
                   lambda v: v.update(expected_sha256="0" * 64), lambda v: v.update(unknown=True)]
        for change in changes:
            value = self.probe_report()
            change(value)
            self.assertFalse(fixture.validate_probe_report(value, 1048576))

    def test_probe_command_is_fixed_and_drops_root(self) -> None:
        command = fixture.probe_command("/probe", 123, 456, "actor", Path("/actor"), 8388608, 120)
        self.assertEqual(command[:9], ["setpriv", "--reuid", "123", "--regid", "456", "--clear-groups", "--no-new-privs", "--", "/probe"])
        self.assertEqual(command[9:], ["--run", "--gateway", "169.254.91.2:6801", "--target", "169.254.91.3",
                                      "--ssh-user", "actor", "--identity-file", "/actor/userkey",
                                      "--host-key-file", "/actor/hostkey.pub", "--payload-bytes", "8388608", "--timeout", "120s"])
        for timeout in (0, 121):
            with self.assertRaises(ValueError):
                fixture.probe_command("/probe", 123, 456, "actor", Path("/actor"), 1048576, timeout)

    def test_probe_execution_checks_exit_and_report_without_native_process(self) -> None:
        for exitcode, corrupt, expected in ((0, False, True), (7, False, False), (0, True, False)):
            value = self.probe_report()
            if corrupt:
                value["cleanup"] = False
            process = Mock(returncode=exitcode)
            process.poll.return_value = exitcode
            selector = Mock()
            selector.__enter__ = Mock(return_value=selector)
            selector.__exit__ = Mock(return_value=False)
            selector.get_map.side_effect = [True, True, False]
            selector.select.return_value = [(Mock(fd=42), 1)]
            with patch.object(fixture, "STOP", False), patch.object(fixture.subprocess, "Popen", return_value=process), patch.object(
                fixture.selectors, "DefaultSelector", return_value=selector
            ), patch.object(fixture.os, "read", side_effect=[json.dumps(value).encode(), b""]), patch.object(
                fixture.os, "open", return_value=99
            ), patch.object(fixture.os, "fdopen", return_value=contextlib.nullcontext(io.BytesIO())), patch.object(fixture, "debug_error"):
                result = fixture.execute_probe(["mock-only"], Path("/not-used"), 1048576, 30)
            self.assertEqual(result["success"], expected)
            self.assertEqual(result["exitcode"], exitcode)
            self.assertTrue(result["process_reaped"])

    def test_cancelled_probe_never_spawns(self) -> None:
        with patch.object(fixture, "STOP", True), patch.object(fixture.subprocess, "Popen") as spawn, patch.object(
            fixture.os, "open", return_value=99
        ), patch.object(fixture.os, "fdopen", return_value=contextlib.nullcontext(io.BytesIO())):
            result = fixture.execute_probe(["mock-only"], Path("/not-used"), 1048576, 30)
        spawn.assert_not_called()
        self.assertFalse(result["success"])
        self.assertEqual(result["code"], "probe-cancelled")

    def test_probe_success_cannot_hide_fixture_cleanup_failure_or_cancellation(self) -> None:
        report = {"probe_result": {"success": True}, "cleanup_errors": []}
        with patch.object(fixture, "STOP", False):
            self.assertTrue(fixture.overall_success(report, True))
            report["cleanup_errors"].append("interfaces-unconfirmed")
            self.assertFalse(fixture.overall_success(report, True))
            self.assertTrue(report["probe_result"]["success"])
            self.assertFalse(fixture.overall_success({"fixture_ready": True, "cleanup_errors": []}, True))
        with patch.object(fixture, "STOP", True):
            self.assertFalse(fixture.overall_success({"probe_result": {"success": True}}, True))

    def test_active_probe_cancel_waits_for_actor_process(self) -> None:
        process = Mock(returncode=-2)
        process.poll.side_effect = [None, -2]
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=False)
        selector.get_map.return_value = True
        def cancel(timeout: float) -> list:
            fixture.STOP = True
            return []
        selector.select.side_effect = cancel
        with patch.object(fixture, "STOP", False), patch.object(fixture.subprocess, "Popen", return_value=process), patch.object(
            fixture.selectors, "DefaultSelector", return_value=selector
        ), patch.object(fixture, "stop_process", return_value={"returncode": -2, "forced": False}) as stop, patch.object(
            fixture.os, "open", return_value=99
        ), patch.object(fixture.os, "fdopen", return_value=contextlib.nullcontext(io.BytesIO())):
            result = fixture.execute_probe(["mock-only"], Path("/not-used"), 1048576, 30)
        stop.assert_called_once_with(process, group=True)
        self.assertFalse(result["success"])
        self.assertTrue(result["process_reaped"])
        self.assertEqual(result["code"], "probe-cancelled")

    def test_probe_binary_preflight_checks_actor_permissions(self) -> None:
        with self.assertRaises(ValueError):
            fixture.validate_probe_binary("relative", 123, 456)
        for mode, allowed in ((stat.S_IFREG | 0o755, True), (stat.S_IFREG | 0o700, False),
                              (stat.S_IFDIR | 0o755, False)):
            with patch.object(Path, "resolve", return_value=Path("/safe/probe")), patch.object(
                Path, "stat", return_value=Mock(st_uid=0, st_gid=0, st_mode=mode)
            ):
                if allowed:
                    fixture.validate_probe_binary("/safe/probe", 123, 456)
                else:
                    with self.assertRaises(ValueError):
                        fixture.validate_probe_binary("/safe/probe", 123, 456)
        with patch.object(Path, "resolve", return_value=Path("/safe/probe")), patch.object(
            Path, "stat", side_effect=[Mock(st_uid=0, st_gid=0, st_mode=stat.S_IFREG | 0o755),
                                      Mock(st_uid=0, st_gid=0, st_mode=stat.S_IFDIR | 0o700)]
        ):
            with self.assertRaisesRegex(ValueError, "traverse"):
                fixture.validate_probe_binary("/safe/probe", 123, 456)

    def test_trusted_tool_ignores_hostile_path_and_rejects_writable_binary(self) -> None:
        safe = Mock(st_uid=0, st_mode=stat.S_IFREG | 0o755)
        with patch.dict(fixture.os.environ, {"PATH": "/attacker"}), patch.object(Path, "is_file", return_value=True), patch.object(
            fixture.os, "access", return_value=True
        ), patch.object(Path, "resolve", side_effect=lambda **kwargs: Path("/usr/sbin/ip")), patch.object(Path, "stat", return_value=safe):
            self.assertEqual(REAL_TRUSTED_TOOL("ip"), "/usr/sbin/ip")
        for uid, mode in ((123, stat.S_IFREG | 0o755), (0, stat.S_IFREG | 0o775)):
            with patch.object(Path, "is_file", return_value=True), patch.object(fixture.os, "access", return_value=True), patch.object(
                Path, "resolve", return_value=Path("/usr/sbin/ip")
            ), patch.object(Path, "stat", return_value=Mock(st_uid=uid, st_mode=mode)):
                with self.assertRaises(RuntimeError):
                    REAL_TRUSTED_TOOL("ip")

    def test_system_command_pins_nested_tools_but_preserves_actor_binary(self) -> None:
        with patch.object(fixture, "trusted_path", return_value="/safe/python") as python:
            self.assertEqual(REAL_SYSTEM_COMMAND(["unshare", "-n", "--", sys.executable, "script"]),
                             ["/usr/bin/unshare", "-n", "--", "/safe/python", "script"])
            python.assert_called_once_with(Path(sys.executable))
        self.assertEqual(REAL_SYSTEM_COMMAND(["nsenter", "--net=/private", "--", "ip", "link"]),
                         ["/usr/bin/nsenter", "--net=/private", "--", "/usr/bin/ip", "link"])
        self.assertEqual(REAL_SYSTEM_COMMAND(["setpriv", "--reuid", "123", "--", "/actor/fake"])[-1], "/actor/fake")
        self.assertEqual(set(fixture.SYSTEM_ENV), {"PATH", "LC_ALL"})
        self.assertNotIn("/attacker", fixture.SYSTEM_ENV["PATH"])

    def test_service_log_budget_keeps_draining_after_full(self) -> None:
        capture = fixture.ServiceLog(Mock(), 99)
        with patch.object(fixture, "SERVICE_LOG_LIMIT", 8), patch.object(fixture.os, "write", side_effect=lambda fd, data: len(data)) as write:
            capture.consume(b"012345")
            capture.consume(b"6789")
            capture.consume(b"abcdefgh")
        self.assertEqual(capture.report["written_bytes"], 8)
        self.assertEqual(capture.report["dropped_bytes"], 10)
        self.assertTrue(capture.report["truncated"])
        self.assertEqual(write.call_count, 2)

    def test_full_service_log_still_reads_through_eof(self) -> None:
        capture = fixture.ServiceLog(Mock(), 99)
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=False)
        selector.select.return_value = [(Mock(fd=42), 1)]
        with patch.object(fixture, "SERVICE_LOG_LIMIT", 8), patch.object(fixture.selectors, "DefaultSelector", return_value=selector), patch.object(
            fixture.os, "set_blocking"
        ), patch.object(fixture.os, "read", side_effect=[b"012345", b"6789", b"abcdefgh", b""]) as read, patch.object(
            fixture.os, "write", side_effect=lambda fd, block: len(block)
        ), patch.object(fixture.os, "close"):
            capture.drain()
        self.assertEqual(read.call_count, 4)
        self.assertTrue(capture.report["eof"])
        self.assertEqual(capture.report["written_bytes"], 8)
        self.assertEqual(capture.report["dropped_bytes"], 10)

    def test_service_log_eof_and_stop_join_without_waiting_for_descendant(self) -> None:
        capture = fixture.ServiceLog(Mock(), 99)
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=False)
        selector.select.return_value = [(Mock(fd=42), 1)]
        with patch.object(fixture.selectors, "DefaultSelector", return_value=selector), patch.object(
            fixture.os, "set_blocking"
        ), patch.object(fixture.os, "read", return_value=b""), patch.object(fixture.os, "close") as close:
            capture.drain()
        self.assertTrue(capture.report["eof"])
        capture.stream.close.assert_called_once()
        close.assert_called_once_with(99)
        self.assertEqual(capture.finish()["joined"], None)
        capture.worker = Mock()
        capture.worker.is_alive.return_value = False
        capture.report["started"] = True
        result = capture.finish()
        capture.worker.join.assert_called_once_with(timeout=2)
        self.assertTrue(result["joined"])
        self.assertTrue(capture.stop.is_set())

    def test_service_log_stop_exits_when_descendant_keeps_pipe_open(self) -> None:
        capture = fixture.ServiceLog(Mock(), 99)
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=False)
        def stop_without_eof(timeout: float) -> list:
            capture.stop.set()
            return []
        selector.select.side_effect = stop_without_eof
        with patch.object(fixture.selectors, "DefaultSelector", return_value=selector), patch.object(
            fixture.os, "set_blocking"
        ), patch.object(fixture.os, "read") as read, patch.object(fixture.os, "close") as close:
            capture.drain()
        read.assert_not_called()
        close.assert_called_once_with(99)
        self.assertFalse(capture.report["eof"])
        self.assertNotIn("error", capture.report)

    def test_namespace_disappeared_is_not_match(self) -> None:
        with patch.object(fixture.os, "readlink", side_effect=FileNotFoundError):
            self.assertFalse(fixture.namespace_matches(Path("/unused"), "net:[1]"))


if __name__ == "__main__":
    unittest.main()
