#!/usr/bin/env python3
"""Opt-in L3 kernel netem check in an externally created, lo-only netns."""

from __future__ import annotations

import argparse
import copy
import grp
import json
import os
import pwd
import re
import selectors
import signal
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from types import FrameType
from typing import Any, BinaryIO


SYSTEM_DIRS = ("/usr/sbin", "/usr/bin", "/sbin", "/bin")
SYSTEM_ENV = {"PATH": ":".join(SYSTEM_DIRS), "LC_ALL": "C"}
COMMAND_TIMEOUT = 5.0
TEST_TIMEOUT = 155.0
WORK_BUDGET = 340.0
CLEANUP_BUDGET = 25.0
TOTAL_BUDGET = 380.0
MAX_SYSTEM_OUTPUT = 1 << 20
MAX_TEST_OUTPUT = 4 << 20
TAG = "L3_NETEM_RESULT "
COUNTERS = ("bytes", "packets", "drops", "overlimits", "requeues", "backlog", "qlen")


class CheckError(RuntimeError):
    pass


def validate_links(links: Any) -> dict[str, Any]:
    if not isinstance(links, list) or len(links) != 1:
        raise CheckError("namespace_not_lo_only")
    link = links[0]
    if not isinstance(link, dict) or link.get("ifname") != "lo" or link.get("link_type") != "loopback":
        raise CheckError("namespace_not_loopback")
    if type(link.get("ifindex")) is not int or not isinstance(link.get("flags"), list):
        raise CheckError("invalid_link_identity")
    return link


def noqueue_only(qdiscs: Any) -> bool:
    return (isinstance(qdiscs, list) and len(qdiscs) == 1
            and isinstance(qdiscs[0], dict) and qdiscs[0].get("kind") == "noqueue"
            and qdiscs[0].get("handle") == "0:" and qdiscs[0].get("root") is True)


def owned_tree(qdiscs: Any, netem_owned: bool) -> bool:
    if not isinstance(qdiscs, list):
        return False
    roots = [q for q in qdiscs if isinstance(q, dict) and q.get("root") is True]
    if len(roots) != 1:
        return False
    root = roots[0]
    if root.get("kind") != "prio" or root.get("handle") != "1:":
        return False
    options = root.get("options", {})
    if not isinstance(options, dict):
        return False
    expected_options: dict[str, Any] = {"bands": 3, "priomap": [0] * 16}
    if "multiqueue" in options:
        if options["multiqueue"] is not False:
            return False
        expected_options["multiqueue"] = False
    if options != expected_options:
        return False
    netems = 0
    defaults: set[str] = set()
    for qdisc in qdiscs:
        if qdisc is root:
            continue
        if not isinstance(qdisc, dict):
            return False
        if qdisc.get("kind") == "netem" and qdisc.get("handle") == "30:" and qdisc.get("parent") == "1:3":
            netems += 1
        elif (qdisc.get("kind") in ("pfifo", "bfifo") and qdisc.get("handle") == "0:"
              and qdisc.get("parent") in ("1:1", "1:2", "1:3")):
            parent = qdisc["parent"]
            if parent in defaults or netem_owned and parent == "1:3":
                return False
            defaults.add(parent)
        else:
            return False
    return netems == int(netem_owned)


def netem_counters(qdiscs: Any) -> dict[str, int]:
    if not isinstance(qdiscs, list):
        raise CheckError("invalid_qdisc_json")
    matches = [q for q in qdiscs if isinstance(q, dict) and q.get("kind") == "netem" and q.get("handle") == "30:"]
    if len(matches) != 1 or matches[0].get("parent") != "1:3":
        raise CheckError("netem_identity_mismatch")
    result = {}
    for key in COUNTERS:
        value = matches[0].get(key)
        if type(value) is not int or value < 0:
            raise CheckError("missing_netem_counter_" + key)
        result[key] = value
    return result


def test_results(text: str, returncode: int, uid: int, gid: int) -> list[dict[str, Any]]:
    lines = text.splitlines()
    if returncode != 0 or "PASS" not in lines:
        raise CheckError("test_failed_or_missing_pass")
    if not any(re.match(r"^--- PASS: TestL3ExternalNetem(?:\s|$)", line) for line in lines):
        raise CheckError("missing_external_test_pass")
    if any(re.match(r"^\s*--- SKIP:", line) for line in lines):
        raise CheckError("test_skipped")
    results = []
    required = {"schema", "bytes", "sha256", "expected_sha256", "active_after", "workers_joined", "uid", "gid", "path"}
    for line in lines:
        if TAG not in line:
            continue
        try:
            result = json.loads(line.split(TAG, 1)[1])
        except (ValueError, TypeError) as exc:
            raise CheckError("invalid_result_json") from exc
        if not isinstance(result, dict) or not required.issubset(result):
            raise CheckError("incomplete_result")
        for name, value in (("schema", 1), ("uid", uid), ("gid", gid), ("active_after", 0)):
            if type(result[name]) is not int or result[name] != value:
                raise CheckError("result_mismatch_" + name)
        if type(result["bytes"]) is not int or result["bytes"] not in (1 << 20, 8 << 20):
            raise CheckError("invalid_result_bytes")
        if (result["workers_joined"] is not True or result["path"] not in ("embedded", "relay")
                or not isinstance(result["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", result["sha256"])
                or result["sha256"] != result["expected_sha256"]):
            raise CheckError("result_hash_or_cleanup_mismatch")
        results.append({key: result[key] for key in sorted(required)})
    if not results:
        raise CheckError("missing_result_tag")
    if len(results) != 2 or {result["bytes"] for result in results} != {1 << 20, 8 << 20}:
        raise CheckError("missing_or_duplicate_payload_result")
    return results


def trusted_tool(name: str) -> str:
    for directory in SYSTEM_DIRS:
        path = Path(directory) / name
        if not path.is_file() or not os.access(path, os.X_OK):
            continue
        resolved = path.resolve(strict=True)
        for component in (resolved, *resolved.parents):
            info = component.stat()
            if info.st_uid != 0 or info.st_mode & 0o022:
                raise CheckError("untrusted_system_tool_" + name)
        return str(resolved)
    raise CheckError("missing_system_tool_" + name)


def validate_run_args(args: argparse.Namespace) -> None:
    if args.actor_uid is None or args.actor_gid is None or args.actor_uid <= 0 or args.actor_gid <= 0:
        raise CheckError("nonroot_actor_required")
    try:
        account = pwd.getpwuid(args.actor_uid)
        grp.getgrgid(args.actor_gid)
    except KeyError as exc:
        raise CheckError("actor_account_missing") from exc
    if account.pw_gid != args.actor_gid:
        raise CheckError("actor_primary_gid_mismatch")
    if not args.test_binary or not args.output:
        raise CheckError("binary_and_output_required")
    binary, output = Path(args.test_binary), Path(args.output)
    if not binary.is_absolute() or not output.is_absolute() or ".." in output.parts:
        raise CheckError("absolute_paths_required")
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise CheckError("test_binary_not_executable")
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise CheckError("new_output_directory_required")
    if output.parent.resolve() != output.parent:
        raise CheckError("output_parent_symlink")
    args.test_binary = str(binary.resolve(strict=True))


@dataclass
class Child:
    process: subprocess.Popen[bytes]
    audit: dict[str, Any]
    signals: list[str] = field(default_factory=list)
    stopped: bool = False

    def stop(self) -> None:
        if self.stopped:
            return
        if self.process.poll() is not None:
            # A reaped PID/PGID may already belong to an unrelated process.
            self.audit.update(returncode=self.process.returncode, signals=self.signals, joined=True, process_group=self.process.pid)
            self.stopped = True
            return
        # Only groups created by this runner with start_new_session are signaled.
        for sig in (signal.SIGTERM, signal.SIGKILL):
            if self.process.poll() is not None:
                break
            try:
                os.killpg(self.process.pid, sig)
                self.signals.append(sig.name)
            except ProcessLookupError:
                break
            try:
                self.process.wait(timeout=2)
                break
            except subprocess.TimeoutExpired:
                continue
        self.process.wait(timeout=2)
        self.audit.update(returncode=self.process.returncode, signals=self.signals, joined=True, process_group=self.process.pid)
        self.stopped = True


class Harness:
    def __init__(self, args: argparse.Namespace, report: dict[str, Any]) -> None:
        self.args = args
        self.report = report
        self.started = time.monotonic()
        self.deadline = self.started + WORK_BUDGET
        self.namespace: tuple[int, int] | None = None
        self.ifindex: int | None = None
        self.original_up: bool | None = None
        self.lo_changed = False
        self.qdisc_owned = False
        self.netem_owned = False
        self.initial_default_confirmed = False
        self.lo_attempted = False
        self.qdisc_attempted = False
        self.netem_attempted = False
        self.netem_change_attempted = False
        self.filter_attempted = False
        self.netem_options: list[dict[str, Any]] = []
        self.qdisc_remaining: bool | None = False
        self.lo_remaining: bool | None = False
        self.children: list[Child] = []
        self.tools: dict[str, str] = {}
        self.interrupted = False
        self.cleaning = False
        self.output_ready = False

    def private_file(self, name: str) -> BinaryIO:
        fd = os.open(Path(self.args.output) / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        return os.fdopen(fd, "wb")

    def interrupted_handler(self, signum: int, frame: FrameType | None) -> None:
        self.interrupted = True
        self.report["signal"] = signum

    def capture(self, command: list[str], timeout: float, limit: int, name: str, test: bool = False) -> bytes:
        if self.interrupted and not self.cleaning:
            raise CheckError("interrupted")
        remaining = min(timeout, self.deadline - time.monotonic())
        if remaining <= 0:
            raise CheckError("total_budget_expired")
        log_name = f"{len(self.report['commands']) + 1:03d}-{name}.log"
        audit: dict[str, Any] = {"argv": command, "returncode": None, "joined": False, "private_log": log_name}
        self.report["commands"].append(audit)
        env = dict(SYSTEM_ENV)
        if test:
            env["RENET_MANUAL_L3_NETEM"] = "1"
        with self.private_file(log_name) as log:
            process = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                       env=env, cwd="/", start_new_session=True)
            child = Child(process, audit)
            self.children.append(child)
            assert process.stdout is not None
            captured = bytearray()
            deadline = time.monotonic() + remaining
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ)
                    while selector.get_map():
                        if self.interrupted and not self.cleaning:
                            raise CheckError("interrupted")
                        left = deadline - time.monotonic()
                        if left <= 0:
                            raise CheckError("process_timeout")
                        for key, _ in selector.select(min(left, 0.2)):
                            chunk = os.read(key.fd, 65536)
                            if not chunk:
                                selector.unregister(key.fileobj)
                                continue
                            if len(captured) + len(chunk) > limit:
                                raise CheckError("process_output_limit")
                            captured.extend(chunk)
                            log.write(chunk)
                process.wait(timeout=max(0.001, deadline - time.monotonic()))
                audit["returncode"] = process.returncode
                audit["output_bytes"] = len(captured)
            except Exception as exc:
                audit["error"] = str(exc) if isinstance(exc, CheckError) else type(exc).__name__
                raise
            finally:
                audit["output_bytes"] = len(captured)
                try:
                    child.stop()
                finally:
                    process.stdout.close()
        if process.returncode != 0:
            raise CheckError("test_process_failed" if test else "system_command_failed")
        return bytes(captured)

    def system_json(self, command: list[str], name: str) -> Any:
        try:
            return json.loads(self.capture(command, COMMAND_TIMEOUT, MAX_SYSTEM_OUTPUT, name))
        except ValueError as exc:
            raise CheckError("invalid_system_json") from exc

    def guard(self) -> dict[str, Any]:
        if os.getuid() != 0 or os.geteuid() != 0:
            raise CheckError("isolated_root_required")
        current, initial = Path("/proc/self/ns/net").stat(), Path("/proc/1/ns/net").stat()
        identity = (current.st_dev, current.st_ino)
        if identity == (initial.st_dev, initial.st_ino) or self.namespace is not None and identity != self.namespace:
            raise CheckError("network_namespace_identity_mismatch")
        link = validate_links(self.system_json([self.tools["ip"], "-j", "link", "show"], "ip-links"))
        if self.ifindex is not None and self.ifindex != link["ifindex"]:
            raise CheckError("loopback_identity_mismatch")
        if self.namespace is None:
            self.namespace, self.ifindex = identity, link["ifindex"]
            self.original_up = "UP" in link["flags"]
            self.report["namespace"] = {"device": identity[0], "inode": identity[1], "pid1_inode": initial.st_ino,
                                        "lo_ifindex": self.ifindex, "lo_original_up": self.original_up}
        return link

    def tc(self, args: list[str]) -> bytes:
        # guard only invokes ip, so every tc read/write has an independent guard.
        self.guard()
        return self.capture([self.tools["tc"], *args], COMMAND_TIMEOUT, MAX_SYSTEM_OUTPUT, "tc")

    def qdiscs(self) -> Any:
        try:
            return json.loads(self.tc(["-s", "-j", "qdisc", "show", "dev", "lo"]))
        except ValueError as exc:
            raise CheckError("invalid_tc_json") from exc

    def set_lo(self, up: bool) -> None:
        self.guard()
        self.capture([self.tools["ip"], "link", "set", "dev", "lo", "up" if up else "down"],
                     COMMAND_TIMEOUT, MAX_SYSTEM_OUTPUT, "ip-state")

    def setup(self) -> None:
        self.guard()
        initial = self.qdiscs()
        empty_down = self.original_up is False and initial == []
        if not noqueue_only(initial) and not empty_down:
            raise CheckError("preexisting_qdisc")
        self.initial_default_confirmed = True
        self.report["initial_qdisc_state"] = "empty_down" if empty_down else "noqueue"
        if self.original_up is False:
            self.lo_attempted = True
            self.lo_remaining = None
            self.set_lo(True)
            self.lo_changed = True
            self.lo_remaining = True
            self.report["lo_brought_up"] = True
            # A fresh, down lo may have no qdisc until its first activation.
            if not noqueue_only(self.qdiscs()):
                raise CheckError("post_up_qdisc_not_noqueue")
        self.qdisc_attempted = True
        self.qdisc_remaining = None
        self.tc(["qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio", "bands", "3", "priomap", *(["0"] * 16)])
        self.qdisc_owned = True
        self.qdisc_remaining = True
        self.netem_attempted = True
        self.tc(["qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "limit", "10000", "delay", "0ms", "loss", "0%"])
        self.netem_owned = True
        self.remember_netem(self.qdiscs())
        self.filter_attempted = True
        self.tc(["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
                 "match", "ip", "protocol", "6", "0xff", "flowid", "1:3"])
        configured = self.qdiscs()
        if not owned_tree(configured, True) or not self.verified_tree(configured):
            raise CheckError("configured_qdisc_identity_mismatch")

    def remember_netem(self, qdiscs: Any) -> None:
        if not owned_tree(qdiscs, True):
            raise CheckError("netem_snapshot_identity_mismatch")
        qdisc = next(q for q in qdiscs if q.get("kind") == "netem")
        options = qdisc.get("options")
        if not isinstance(options, dict) or options.get("limit") != 10000:
            raise CheckError("netem_snapshot_options_missing")
        if options not in self.netem_options:
            self.netem_options.append(copy.deepcopy(options))
        self.report["confirmed_netem_options"] = copy.deepcopy(self.netem_options)

    def verified_tree(self, qdiscs: Any) -> bool:
        if owned_tree(qdiscs, False):
            return True
        if not self.netem_attempted or not owned_tree(qdiscs, True):
            return False
        qdisc = next(q for q in qdiscs if q.get("kind") == "netem")
        return qdisc.get("options") in self.netem_options

    def profile(self, name: str) -> None:
        existing = self.qdiscs()
        if not owned_tree(existing, True) or not self.verified_tree(existing):
            raise CheckError("profile_qdisc_identity_mismatch")
        entry: dict[str, Any] = {"name": name, "ok": False, "one_way_delay_ms": 200 if name == "degraded" else 0,
                                 "configured_loss_percent": 1 if name == "degraded" else 0}
        self.report["profiles"].append(entry)
        self.netem_change_attempted = True
        self.tc(["qdisc", "change", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "limit", "10000",
                 "delay", f"{entry['one_way_delay_ms']}ms", "loss", f"{entry['configured_loss_percent']}%"])
        configured = self.qdiscs()
        self.remember_netem(configured)
        before = netem_counters(configured)
        entry["counters_before"] = before
        command = [self.tools["setpriv"], f"--reuid={self.args.actor_uid}", f"--regid={self.args.actor_gid}",
                   "--clear-groups", "--no-new-privs", "--bounding-set=-all", self.args.test_binary,
                   "-test.run", "^TestL3ExternalNetem$", "-test.count=1", "-test.v", "-test.timeout=150s"]
        try:
            output = self.capture(command, TEST_TIMEOUT, MAX_TEST_OUTPUT, name, test=True)
            entry["results"] = test_results(output.decode("utf-8", errors="strict"), 0, self.args.actor_uid, self.args.actor_gid)
        finally:
            after = netem_counters(self.qdiscs())
            entry["counters_after"] = after
            entry["counter_delta"] = {key: after[key] - before[key] for key in COUNTERS}
        delta = entry["counter_delta"]
        if any(delta[key] < 0 for key in ("packets", "bytes", "drops", "overlimits", "requeues")):
            raise CheckError("kernel_counters_regressed")
        if delta["packets"] <= 0 or delta["bytes"] <= 0 or name == "degraded" and delta["drops"] <= 0:
            raise CheckError("missing_kernel_packet_or_drop_evidence")
        entry["ok"] = True

    def cleanup(self) -> list[str]:
        self.cleaning = True
        self.deadline = min(self.started + TOTAL_BUDGET, time.monotonic() + CLEANUP_BUDGET)
        errors = []
        for child in reversed(self.children):
            try:
                child.stop()
            except Exception:
                errors.append("child_cleanup_failed")
        if self.qdisc_attempted or self.qdisc_owned:
            self.qdisc_remaining = None
            try:
                if not self.initial_default_confirmed:
                    raise CheckError("cleanup_initial_state_unconfirmed")
                link = self.guard()
                qdiscs = self.qdiscs()
                absent = noqueue_only(qdiscs) or (qdiscs == [] and self.original_up is False and "UP" not in link["flags"])
                if absent:
                    self.qdisc_remaining = False
                    self.qdisc_owned = False
                    self.netem_owned = False
                    self.report["qdisc_cleanup_state"] = "verified_absent"
                else:
                    if not self.verified_tree(qdiscs):
                        raise CheckError("cleanup_qdisc_options_or_identity_unconfirmed")
                    if owned_tree(qdiscs, True):
                        self.report["cleanup_counters"] = netem_counters(qdiscs)
                    mutation_error = None
                    try:
                        self.tc(["qdisc", "del", "dev", "lo", "root", "handle", "1:"])
                    except Exception as exc:
                        mutation_error = str(exc) if isinstance(exc, CheckError) else "qdisc_delete_failed"
                    # Even a timed-out command may have already changed kernel state.
                    if not noqueue_only(self.qdiscs()):
                        self.qdisc_remaining = True
                        raise CheckError("cleanup_qdisc_remains")
                    self.qdisc_remaining = False
                    self.qdisc_owned = False
                    self.netem_owned = False
                    self.report["qdisc_removed"] = True
                    self.report["qdisc_cleanup_state"] = "verified_removed"
                    if mutation_error is not None:
                        errors.append(mutation_error)
            except Exception as exc:
                self.report["qdisc_cleanup_state"] = "unconfirmed"
                errors.append(str(exc) if isinstance(exc, CheckError) else "qdisc_cleanup_failed")
        if self.lo_attempted or self.lo_changed:
            self.lo_remaining = None
            try:
                link = self.guard()
                if self.original_up is not False:
                    raise CheckError("cleanup_original_lo_state_unconfirmed")
                mutation_error = None
                if "UP" in link["flags"]:
                    try:
                        self.set_lo(False)
                    except Exception as exc:
                        mutation_error = str(exc) if isinstance(exc, CheckError) else "loopback_restore_failed"
                    link = self.guard()
                if "UP" in link["flags"]:
                    self.lo_remaining = True
                    raise CheckError("loopback_not_restored")
                self.report["lo_restored"] = True
                self.lo_remaining = False
                self.lo_changed = False
                if mutation_error is not None:
                    errors.append(mutation_error)
            except Exception as exc:
                errors.append(str(exc) if isinstance(exc, CheckError) else "loopback_cleanup_failed")
        self.report["mutation_attempts"] = {"lo_up": self.lo_attempted, "root_prio_add": self.qdisc_attempted,
                                             "netem_add": self.netem_attempted, "filter_add": self.filter_attempted,
                                             "netem_change": self.netem_change_attempted}
        return errors


def arguments(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", help="Explicitly run inside an already isolated root netns")
    parser.add_argument("--test-binary", help="Absolute path to the compiled integration.test")
    parser.add_argument("--output", help="New absolute private report/log directory")
    parser.add_argument("--actor-uid", type=int, help="Existing non-root account UID")
    parser.add_argument("--actor-gid", type=int, help="Account primary GID; supplementary groups are cleared")
    parser.add_argument("--profile", choices=("baseline", "degraded", "all"), default="all")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = arguments(argv)
    if not args.run:
        print(json.dumps({"dry_run": True, "profile": args.profile, "writes": False, "processes": False,
                          "requires": "externally isolated root netns containing only lo with noqueue; existing ordinary actor",
                          "work_budget_s": WORK_BUDGET, "cleanup_budget_s": CLEANUP_BUDGET, "total_budget_s": TOTAL_BUDGET}))
        return 0
    report: dict[str, Any] = {"schema": 1, "ok": False, "stage": "preflight", "commands": [], "profiles": [],
                              "qdisc_removed": False, "cleanup_errors": [], "total_budget_s": TOTAL_BUDGET,
                              "evidence_scope": "Loopback TCP Hub traffic only; 200ms per direction, 1% configured loss. Observed kernel counters are not a measured loss ratio or a performance conclusion."}
    harness = Harness(args, report)
    old_handlers: dict[int, Any] = {}
    report_written = False
    try:
        validate_run_args(args)
        harness.tools = {name: trusted_tool(name) for name in ("ip", "tc", "setpriv")}
        Path(args.output).mkdir(mode=0o700)
        harness.output_ready = True
        os.chmod(args.output, 0o700)
        for sig in (signal.SIGINT, signal.SIGTERM):
            old_handlers[sig] = signal.signal(sig, harness.interrupted_handler)
        report["stage"] = "network_setup"
        harness.setup()
        for profile in (("baseline", "degraded") if args.profile == "all" else (args.profile,)):
            report["stage"] = profile
            harness.profile(profile)
        if harness.interrupted:
            raise CheckError("interrupted")
        report.update(ok=True, stage="completed")
    except Exception as exc:
        report["error"] = str(exc) if isinstance(exc, CheckError) else type(exc).__name__
    finally:
        report["cleanup_errors"] = harness.cleanup()
        report["resources_remaining"] = {"owned_qdisc": harness.qdisc_remaining, "lo_state_changed": harness.lo_remaining}
        if report["cleanup_errors"] or harness.interrupted:
            report["ok"] = False
        if harness.interrupted:
            report.setdefault("error", "interrupted")
        report["elapsed_s"] = time.monotonic() - harness.started
        if harness.output_ready:
            try:
                with harness.private_file("report.json") as output:
                    output.write((json.dumps(report, indent=2, sort_keys=True) + "\n").encode())
                report_written = True
            except Exception:
                report["ok"] = False
                report["cleanup_errors"].append("report_write_failed")
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)
    print(json.dumps({"ok": report["ok"], "stage": report["stage"], "error": report.get("error"),
                      "cleanup_errors": report["cleanup_errors"], "report_written": report_written}))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
