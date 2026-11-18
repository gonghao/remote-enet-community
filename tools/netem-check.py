#!/usr/bin/env python3
"""Manual Linux netns acceptance tool; never part of the default test suite."""

from __future__ import annotations

import argparse
import json
import os
import re
import selectors
import shlex
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from types import FrameType
from typing import Any, BinaryIO


VIN = "WBAXXXXXXX0000001"
HUB = "127.0.0.1:47000"
TOOL_ADDRESS = ("127.0.0.1", 26801)
IO_TIMEOUT = 15.0
COMMAND_TIMEOUT = 8.0
WAIT_TIMEOUT = 75.0
BLACKHOLE_OBSERVATION = 3.0
REPLAY_OBSERVATION = 2.0
MAX_LOG_BYTES = 8 * 1024 * 1024


def frame(control: int, body: bytes) -> bytes:
    return struct.pack("!IH", len(body), control) + body


REQUEST = frame(1, bytes.fromhex("f41022f190"))
ACK = frame(2, bytes.fromhex("f41022f190"))
RESPONSE = frame(1, bytes.fromhex("10f462f190") + VIN.encode("ascii"))


def redact(value: str) -> str:
    return re.sub(r"(?i)renet1:[^\s\"'<>]+", "[redacted invitation]", value)


def percentile(samples: list[float], fraction: float) -> float | None:
    if not samples:
        return None
    ordered = sorted(samples)
    position = (len(ordered) - 1) * fraction
    lower = int(position)
    upper = min(lower + 1, len(ordered) - 1)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def parse_log(text: str) -> list[dict[str, str]]:
    events = []
    for line in text.splitlines():
        try:
            fields = shlex.split(line)
        except ValueError:
            continue
        events.append(dict(token.split("=", 1) for token in fields if "=" in token))
    return events


def fake_counts(events: list[dict[str, str]]) -> dict[str, int]:
    frames = [event for event in events if event.get("msg") == "frame receive"]
    return {
        "vin_requests": sum(event.get("hex") == REQUEST.hex() for event in frames),
        "other_frames": sum(event.get("hex") != REQUEST.hex() for event in frames),
        "connections": sum(event.get("msg") == "connection accepted" for event in events),
    }


def netem_counters(qdiscs: list[dict[str, Any]]) -> dict[str, int]:
    matches = [q for q in qdiscs if q.get("kind") == "netem" and q.get("handle") == "30:"]
    if len(matches) != 1 or matches[0].get("parent") != "1:3":
        raise RuntimeError("expected exactly one netem 30: attached to 1:3")
    qdisc = matches[0]
    counters = {}
    for name in ("bytes", "packets", "drops", "overlimits", "requeues", "backlog", "qlen"):
        value = qdisc.get(name)
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            raise RuntimeError(f"tc JSON is missing a nonnegative netem {name} counter")
        counters[name] = value
    return counters


def validate_links(links: Any) -> None:
    if not isinstance(links, list) or len(links) != 1:
        raise RuntimeError("refusing network changes: namespace must contain only lo")
    if not isinstance(links[0], dict) or links[0].get("ifname") != "lo" or links[0].get("link_type") != "loopback":
        raise RuntimeError("refusing network changes: sole device must be loopback lo")


def receive_exact(conn: socket.socket, size: int, deadline: float) -> bytes:
    result = bytearray()
    while len(result) < size:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("diagnostic exchange deadline expired")
        conn.settimeout(remaining)
        part = conn.recv(size - len(result))
        if not part:
            raise RuntimeError("diagnostic connection closed before the complete frame")
        result.extend(part)
    return bytes(result)


def tool_worker() -> int:
    # Only this unprivileged process creates diagnostic sockets. The parent
    # retains root for ip/tc and orchestration, never for application traffic.
    if os.geteuid() == 0 or os.getegid() == 0 or os.getgroups():
        return 1
    conn: socket.socket | None = None
    try:
        for line in sys.stdin:
            try:
                command = json.loads(line)
                operation = command["operation"]
                result: dict[str, Any] = {"ok": True}
                if operation == "identity":
                    result.update(uid=os.geteuid(), gid=os.getegid(), groups=os.getgroups())
                elif operation in ("read", "send"):
                    if conn is None:
                        if operation == "send":
                            raise RuntimeError("blackhole request requires the existing tool socket")
                        conn = socket.create_connection(TOOL_ADDRESS, timeout=IO_TIMEOUT)
                    started = time.monotonic()
                    conn.settimeout(IO_TIMEOUT)
                    conn.sendall(REQUEST)
                    if operation == "read":
                        deadline = started + IO_TIMEOUT
                        if receive_exact(conn, len(ACK), deadline) != ACK:
                            raise RuntimeError("ACK raw bytes differ from the read-only VIN fixture")
                        if receive_exact(conn, len(RESPONSE), deadline) != RESPONSE:
                            raise RuntimeError("VIN response raw bytes differ from the fixture")
                        result["latency_ms"] = (time.monotonic() - started) * 1000
                    result["request_bytes"] = len(REQUEST)
                elif operation == "eof":
                    if conn is None:
                        raise RuntimeError("old tool socket is missing")
                    conn.settimeout(IO_TIMEOUT)
                    if conn.recv(1) != b"":
                        raise RuntimeError("old socket retained data instead of EOF")
                    conn.close()
                    conn = None
                    result["eof"] = True
                else:
                    raise RuntimeError("unknown tool worker operation")
            except Exception as exc:
                print(json.dumps({"ok": False, "error": redact(str(exc))}), flush=True)
                return 1
            print(json.dumps(result), flush=True)
        return 0
    finally:
        if conn is not None:
            conn.close()


@dataclass
class Child:
    name: str
    process: subprocess.Popen[bytes]
    log: Path
    signals: list[str] = field(default_factory=list)
    forced: bool = False

    def events(self) -> list[dict[str, str]]:
        if self.log.stat().st_size > MAX_LOG_BYTES:
            raise RuntimeError(f"{self.name} private log exceeded the size limit")
        return parse_log(self.log.read_text(encoding="utf-8", errors="replace"))

    def alive(self) -> None:
        code = self.process.poll()
        if code is not None:
            raise RuntimeError(f"{self.name} exited unexpectedly with code {code}; inspect its private log")

    def stop(self) -> None:
        if self.process.poll() is None and self.name == "tool" and self.process.stdin is not None:
            try:
                self.process.stdin.close()
            except BrokenPipeError:
                pass
            try:
                self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                pass
        for sig, timeout in ((signal.SIGINT, 5), (signal.SIGTERM, 3), (signal.SIGKILL, 3)):
            if self.process.poll() is not None:
                break
            self.signals.append(sig.name)
            self.forced |= sig != signal.SIGINT
            try:
                os.killpg(self.process.pid, sig)
            except ProcessLookupError:
                pass
            try:
                self.process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                continue
        if self.process.poll() is None:
            raise RuntimeError(f"{self.name} did not exit after SIGKILL")
        for stream in (self.process.stdin, self.process.stdout):
            if stream is not None:
                stream.close()


class Harness:
    def __init__(self, args: argparse.Namespace, report: dict[str, Any]) -> None:
        self.args = args
        self.report = report
        self.output = Path(args.output).absolute()
        self.children: list[Child] = []
        self.namespace: int | None = None
        self.qdisc_owned = False
        self.seed_supported: bool | None = None
        self.worker_buffer = bytearray()
        self.started = time.monotonic()
        self.blackhole_started = 0.0

    def private_file(self, name: str) -> BinaryIO:
        fd = os.open(self.output / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.fchown(fd, self.args.actor_uid, self.args.actor_gid)
            return os.fdopen(fd, "wb")
        except BaseException:
            os.close(fd)
            raise

    def prepare_output(self) -> None:
        # A fresh directory avoids overwriting a previous run's keys or logs.
        self.output.mkdir(mode=0o700)
        os.chown(self.output, self.args.actor_uid, self.args.actor_gid)

    def run_system(self, command: list[str]) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            command, stdin=subprocess.DEVNULL, capture_output=True, text=True,
            timeout=COMMAND_TIMEOUT, check=False,
            env={"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C"},
        )

    def system_json(self, command: list[str]) -> Any:
        result = self.run_system(command)
        if result.returncode:
            raise RuntimeError(f"{Path(command[0]).name} query failed: {result.stderr.strip()}")
        return json.loads(result.stdout)

    def guard(self) -> None:
        if os.getuid() != 0 or os.geteuid() != 0:
            raise RuntimeError("UID 0 is required inside a fresh isolated network namespace")
        current = Path("/proc/self/ns/net").stat().st_ino
        initial = Path("/proc/1/ns/net").stat().st_ino
        self.report["namespace"] = {"self_inode": current, "pid1_inode": initial}
        if current == initial or self.namespace is not None and current != self.namespace:
            raise RuntimeError("refusing network changes: not in the expected isolated netns")
        validate_links(self.system_json([self.args.ip_bin, "-j", "link", "show"]))
        self.namespace = current

    def tc(self, args: list[str], allow_failure: bool = False) -> subprocess.CompletedProcess[str]:
        self.guard()
        result = self.run_system([self.args.tc_bin, *args])
        self.report["tc_commands"].append({
            "args": args, "returncode": result.returncode, "stderr": result.stderr.strip(),
        })
        if result.returncode and not allow_failure:
            raise RuntimeError(f"tc failed for {' '.join(args)}: {result.stderr.strip()}")
        return result

    def tc_json(self, args: list[str]) -> Any:
        return json.loads(self.tc(args).stdout)

    def setup_network(self) -> None:
        self.guard()
        result = self.run_system([self.args.ip_bin, "link", "set", "dev", "lo", "up"])
        if result.returncode:
            raise RuntimeError(f"cannot bring isolated lo up: {result.stderr.strip()}")
        existing = self.tc_json(["-j", "qdisc", "show", "dev", "lo"])
        if any(q.get("kind") != "noqueue" for q in existing):
            raise RuntimeError("refusing to replace a preexisting qdisc in the namespace")
        self.qdisc_owned = True
        # Every default priority uses band 1. Only the two TCP filters below
        # enter band 3; neither diagnostic tools nor vehicle sockets are WAN.
        self.tc(["qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio",
                 "bands", "3", "priomap", *(["0"] * 16)])
        self.tc(["qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:",
                 "netem", "limit", "10000", "delay", "0ms", "loss", "0%"])
        for priority, direction in ((1, "dport"), (2, "sport")):
            self.tc(["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:",
                     "prio", str(priority), "u32", "match", "ip", "protocol", "6", "0xff",
                     "match", "ip", direction, "47000", "0xffff", "flowid", "1:3"])
        self.snapshot("baseline_configured")

    def profile(self, loss: int) -> None:
        args = ["qdisc", "change", "dev", "lo", "parent", "1:3", "handle", "30:",
                "netem", "limit", "10000", "delay", "100ms", "loss", f"{loss}%"]
        if loss == 1 and self.seed_supported is None:
            result = self.tc([*args, "seed", "7"], allow_failure=True)
            self.seed_supported = result.returncode == 0
            if result.returncode:
                error = result.stderr.lower()
                if "seed" not in error or not any(word in error for word in ("unknown", "what is", "usage", "unrecognized", "illegal")):
                    raise RuntimeError(f"tc seed attempt failed for a reason other than unsupported syntax: {result.stderr.strip()}")
                self.tc(args)
        else:
            self.tc([*args, *(["seed", "7"] if self.seed_supported else [])])
        self.report["netem_seed"] = {"requested": 7, "supported": self.seed_supported}

    def snapshot(self, label: str) -> dict[str, Any]:
        self.guard()
        data = {
            "label": label, "elapsed_s": time.monotonic() - self.started,
            "qdisc": self.tc_json(["-s", "-j", "qdisc", "show", "dev", "lo"]),
            "class": self.tc_json(["-s", "-j", "class", "show", "dev", "lo"]),
            "filter": self.tc_json(["-s", "-j", "filter", "show", "dev", "lo", "parent", "1:"]),
        }
        self.report["tc_snapshots"].append(data)
        data["netem_counters"] = netem_counters(data["qdisc"])
        rules = [entry for entry in data["filter"] if entry.get("options", {}).get("flowid") == "1:3"]
        if len(rules) != 2:
            raise RuntimeError("tc JSON does not show both WAN filters targeting 1:3")
        return data["netem_counters"]

    def spawn(self, name: str, command: list[str], stdout: BinaryIO | int | None = None, worker: bool = False) -> Child:
        with self.private_file(f"{name}.log") as log:
            process = subprocess.Popen(
                command, stdin=subprocess.PIPE if worker else subprocess.DEVNULL,
                stdout=subprocess.PIPE if worker else log if stdout is None else stdout,
                stderr=log, cwd=self.output, user=self.args.actor_uid, group=self.args.actor_gid,
                extra_groups=[], start_new_session=True,
                env={"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
            )
        child = Child(name, process, self.output / f"{name}.log")
        self.children.append(child)
        return child

    def wait_event(self, child: Child, predicate: Callable[[dict[str, str]], bool], timeout: float = WAIT_TIMEOUT, start: int = 0) -> dict[str, str] | None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            child.alive()
            for event in child.events()[start:]:
                if predicate(event):
                    return event
            time.sleep(0.05)
        return None

    def require_event(self, child: Child, message: str) -> dict[str, str]:
        result = self.wait_event(child, lambda event: event.get("msg") == message)
        if result is None:
            raise TimeoutError(f"timed out waiting for {child.name}: {message}")
        return result

    def request_tool(self, child: Child, operation: str) -> dict[str, Any]:
        child.alive()
        assert child.process.stdin is not None and child.process.stdout is not None
        child.process.stdin.write((json.dumps({"operation": operation}) + "\n").encode())
        child.process.stdin.flush()
        deadline = time.monotonic() + IO_TIMEOUT + 5
        with selectors.DefaultSelector() as selector:
            selector.register(child.process.stdout, selectors.EVENT_READ)
            while b"\n" not in self.worker_buffer:
                remaining = deadline - time.monotonic()
                if remaining <= 0 or not selector.select(remaining):
                    raise TimeoutError(f"tool worker timed out during {operation}")
                chunk = os.read(child.process.stdout.fileno(), 65536)
                if not chunk:
                    raise RuntimeError(f"tool worker exited during {operation}; inspect its private log")
                self.worker_buffer.extend(chunk)
                if len(self.worker_buffer) > 65536:
                    raise RuntimeError("tool worker reply exceeded the size limit")
        line, _, remaining_buffer = self.worker_buffer.partition(b"\n")
        self.worker_buffer = bytearray(remaining_buffer)
        reply = json.loads(line)
        if not reply.get("ok"):
            raise RuntimeError(f"tool {operation} failed: {reply.get('error', 'unknown error')}")
        return reply

    def assert_fake(self, fake: Child, expected: int) -> dict[str, int]:
        fake.alive()
        counts = fake_counts(fake.events())
        self.report["fake_counts"] = counts
        if counts["vin_requests"] != expected or counts["other_frames"]:
            raise RuntimeError(f"fake gateway counts differ from expected {expected} read-only requests: {counts}")
        return counts

    def observe_no_request(self, fake: Child, expected: int, seconds: float, peers: list[Child]) -> None:
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            self.assert_fake(fake, expected)
            for child in peers:
                child.alive()
            time.sleep(min(0.05, max(0, deadline - time.monotonic())))

    def peer_test(self, operator: Child, fake: Child, expected: int, label: str) -> None:
        before = self.assert_fake(fake, expected)
        entry: dict[str, Any] = {"stage": label, "fake_before": before}
        self.report["peer_only_tests"].append(entry)
        result = self.wait_event(operator, lambda event: event.get("msg") in ("connection test completed", "connection test failed"), timeout=20)
        if result is None:
            entry["result"] = {"status": "not_observed_within_20s"}
            raise TimeoutError("peer-only connection test did not report completion within 20s")
        else:
            allowed = ("status", "generation", "requested_samples", "samples", "rtt_valid", "rtt_p50", "rtt_p95", "bytes_sent", "bytes_received", "echoed_bytes", "duration", "payload_duration", "echo_bytes_per_second", "timed_out", "interrupted")
            entry["result"] = {key: result[key] for key in allowed if key in result}
            if result.get("msg") != "connection test completed" or result.get("status") != "ok":
                raise RuntimeError("peer-only connection test did not succeed")
            if result.get("samples") != result.get("requested_samples") or int(result.get("samples", "0")) <= 0 or result.get("rtt_valid") != "true":
                raise RuntimeError("peer-only connection test did not complete all RTT samples")
            if any(result.get(key) != "false" for key in ("timed_out", "interrupted")):
                raise RuntimeError("peer-only connection test reported an interruption or timeout")
            # CLI defaults budget 1 MiB per direction including the 32-byte RTT
            # probes; EchoedBytes intentionally counts only the bulk echoes.
            payload = 1 << 20
            bulk = payload - int(result["samples"]) * 32
            if any(int(result.get(key, "0")) != payload for key in ("bytes_sent", "bytes_received")) or int(result.get("echoed_bytes", "0")) != bulk:
                raise RuntimeError("peer-only connection test did not echo the entire payload")
        after = self.assert_fake(fake, expected)
        entry["fake_after"] = after
        if before != after:
            raise RuntimeError("peer-only connection test opened a vehicle connection")
        start = len(operator.events())
        status = self.wait_event(operator, lambda event: event.get("msg") == "runtime status" and event.get("phase") == "connected", timeout=3, start=start)
        if status is None:
            raise RuntimeError("missing operator payload accounting before diagnostic traffic")
        fields = ("payload_bytes_in", "payload_bytes_out", "total_payload_bytes_in", "total_payload_bytes_out")
        entry["vehicle_payload_counters"] = {key: status.get(key) for key in fields}
        if any(status.get(key) != "0" for key in fields):
            raise RuntimeError("peer-only test appeared in vehicle payload counters")

    def wait_blackhole_teardown(self, client: Child, fake: Child, tool: Child, expected: int) -> None:
        # A local close can leave unacknowledged TCP bytes in the kernel. Keep
        # the blackhole until the remote app has closed its old vehicle socket;
        # restoring connectivity earlier can deliver those bytes on the old
        # generation and cannot prove anything about application replay.
        deadline = time.monotonic() + WAIT_TIMEOUT
        accepted = [event for event in fake.events() if event.get("msg") == "connection accepted"]
        if len(accepted) != 1:
            raise RuntimeError("expected one old vehicle connection before blackhole teardown")
        peer = accepted[0]["peer"]
        while time.monotonic() < deadline:
            self.assert_fake(fake, expected)
            client.alive()
            tool.alive()
            ended = next((event for event in client.events() if event.get("msg") == "client connection ended; retrying"), None)
            closed = next((event for event in fake.events() if event.get("msg") == "connection closed" and event.get("peer") == peer), None)
            if ended is not None and closed is not None:
                self.report["blackhole_teardown"] = {
                    "client_disconnected": {key: ended[key] for key in ("time", "error") if key in ended},
                    "vehicle_connection_closed": {key: closed[key] for key in ("time", "peer") if key in closed},
                    "elapsed_since_blackhole_s": time.monotonic() - self.blackhole_started,
                    "fake_counts": self.assert_fake(fake, expected),
                }
                return
            time.sleep(0.05)
        raise TimeoutError("client did not tear down the old vehicle connection while the blackhole remained active")

    def measure(self, tool: Child, fake: Child, label: str, count: int) -> None:
        samples = self.report["measurements_ms"][label]
        for _ in range(count):
            result = self.request_tool(tool, "read")
            samples.append(result["latency_ms"])
            self.report["successful_requests"][label] += 1
            total = sum(self.report["successful_requests"].values())
            self.assert_fake(fake, total)

    def operator(self, name: str) -> Child:
        return self.spawn(name, [
            self.args.renetd_bin, "operator", "--state-dir", str(self.output / "operator-state"),
            "--listen", HUB, "--bind", "127.0.0.1", "--diag-port", "26801", "--ctrl-port", "26811",
            "--disc-port", "26811", "--remote-diag-port", "16801", "--remote-ctrl-port", "16811",
            "--exclusive=false", "--status-interval", "500ms", "--connection-test", "--connection-test-timeout", "15s",
        ])

    def presented(self, operator: Child) -> dict[str, str]:
        event = self.require_event(operator, "operator presenting vehicle")
        expected = {"vin": VIN, "diagnostic": "127.0.0.1:26801", "control": "127.0.0.1:26811", "discovery": "127.0.0.1:26811"}
        if any(event.get(key) != value for key, value in expected.items()):
            raise RuntimeError("operator presentation differs from the fixed loopback topology")
        return {key: event[key] for key in (*expected, "generation")}

    def exercise(self) -> None:
        self.report["stage"] = "network_setup"
        self.setup_network()
        with self.private_file("vin-responder.json") as responder:
            responder.write(json.dumps({"22f190": (bytes.fromhex("62f190") + VIN.encode("ascii")).hex()}).encode())
        self.report["stage"] = "launch_and_pair"
        fake = self.spawn("fakezgw", [
            self.args.fakezgw_bin, "--bind", "127.0.0.2", "--vin", VIN, "--diag-addr", "0x10",
            "--diag-port", "16801", "--ctrl-port", "16811", "--disc-port", "16811",
            "--alive", "0", "--responders", str(self.output / "vin-responder.json"),
        ])
        self.require_event(fake, "fake ZGW listening")
        operator = self.operator("operator-initial")
        self.require_event(operator, "operator ready")
        with self.private_file("invitation.txt") as invitation:
            issuer = self.spawn("invite", [self.args.renetd_bin, "invite", "--state-dir", str(self.output / "operator-state"), "--endpoint", HUB], stdout=invitation)
            if issuer.process.wait(timeout=15) != 0:
                raise RuntimeError("invitation subprocess failed; inspect its private log")
        secret = (self.output / "invitation.txt").read_bytes().strip()
        if not secret.startswith(b"renet1:") or len(secret) > 65536:
            raise RuntimeError("invitation subprocess did not produce a valid invitation prefix")
        del secret
        client = self.spawn("client", [
            self.args.renetd_bin, "client", "--state-dir", str(self.output / "client-state"),
            "--invite-file", str(self.output / "invitation.txt"), "--nic", "lo", "--nic-addr", "127.0.0.1",
            "--discovery-target", "127.0.0.2:16811", "--discovery-timeout", "100ms", "--status-interval", "500ms",
        ])
        first_client = self.require_event(client, "client connected")
        self.report["initial_presentation"] = self.presented(operator)
        self.report["stage"] = "baseline"
        self.peer_test(operator, fake, 0, "baseline")
        tool = self.spawn("tool", [sys.executable, "-I", str(Path(__file__).resolve()), "--_tool-worker"], worker=True)
        identity = self.request_tool(tool, "identity")
        if identity != {"ok": True, "uid": self.args.actor_uid, "gid": self.args.actor_gid, "groups": []}:
            raise RuntimeError("tool worker did not drop all root credentials and supplementary groups")
        self.report["tool_actor"] = identity
        self.measure(tool, fake, "baseline", 3)
        self.snapshot("baseline_completed")
        self.report["stage"] = "degraded"
        self.profile(1)
        before_degraded = self.snapshot("degraded_started")
        self.measure(tool, fake, "degraded", 10)
        after_degraded = self.snapshot("degraded_completed")
        if after_degraded["packets"] <= before_degraded["packets"]:
            raise RuntimeError("netem did not count forwarded WAN packets during degraded requests")
        self.report["degraded_kernel_counts"] = {
            name: after_degraded[name] - before_degraded[name] for name in ("packets", "bytes", "drops")
        }
        self.report["stage"] = "blackhole"
        self.profile(100)
        self.blackhole_started = time.monotonic()
        before_blackhole = self.snapshot("blackhole_started")
        self.report["blackhole_send"] = self.request_tool(tool, "send")
        self.observe_no_request(fake, 13, BLACKHOLE_OBSERVATION, [operator, client, tool])
        after_blackhole = self.snapshot("blackhole_observed")
        dropped = after_blackhole["drops"] - before_blackhole["drops"]
        self.report["blackhole_kernel_drops"] = dropped
        if dropped <= 0:
            raise RuntimeError("100% blackhole produced no observed kernel drops")
        self.report["queued_request_not_delivered_at_blackhole_observation"] = True
        self.report["stage"] = "restart_and_reconnect"
        operator.stop()
        if operator.process.returncode != 0:
            raise RuntimeError("initial operator did not exit cleanly on interrupt")
        self.report["old_tool_connection"] = self.request_tool(tool, "eof")
        self.wait_blackhole_teardown(client, fake, tool, 13)
        self.snapshot("blackhole_old_connection_closed")
        self.profile(1)
        restarted = self.operator("operator-restarted")
        self.require_event(restarted, "operator ready")
        connected = self.wait_event(client, lambda event: event.get("msg") == "client connected" and int(event.get("generation", "0")) > int(first_client["generation"]))
        if connected is None:
            raise TimeoutError("client did not reconnect with a new generation")
        self.report["client_generations"] = [int(first_client["generation"]), int(connected["generation"])]
        self.report["restarted_presentation"] = self.presented(restarted)
        self.assert_fake(fake, 13)
        self.peer_test(restarted, fake, 13, "restarted_degraded")
        self.observe_no_request(fake, 13, REPLAY_OBSERVATION, [restarted, client, tool])
        self.report["stage"] = "recovery"
        self.measure(tool, fake, "recovery", 3)
        self.observe_no_request(fake, 16, REPLAY_OBSERVATION, [restarted, client, tool])
        self.report["queued_not_replayed"] = True
        self.report["final_kernel_counts"] = self.snapshot("recovery_completed")
        self.report["stage"] = "completed"

    def cleanup(self) -> list[str]:
        errors = []
        exits = []
        for child in reversed(self.children):
            try:
                child.stop()
                if child.process.returncode != 0:
                    errors.append(f"{child.name} exit code {child.process.returncode}")
            except Exception as exc:
                errors.append(f"{child.name}: {redact(str(exc))}")
            exits.append({"name": child.name, "pid": child.process.pid, "actor_uid": self.args.actor_uid,
                          "actor_gid": self.args.actor_gid, "extra_groups": [], "returncode": child.process.poll(),
                          "signals": child.signals, "forced": child.forced, "private_log": child.log.name})
        self.report["process_exits"] = exits
        for child in self.children:
            if child.name == "fakezgw":
                try:
                    counts = fake_counts(child.events())
                    self.report["final_fake_counts"] = counts
                    if self.report["queued_not_replayed"] is True and (counts["vin_requests"] != 16 or counts["other_frames"]):
                        self.report["queued_not_replayed"] = False
                        errors.append("fake gateway request count changed during shutdown")
                except Exception as exc:
                    errors.append(f"final fake log counters: {redact(str(exc))}")
        if self.qdisc_owned:
            try:
                self.snapshot("before_cleanup")
            except Exception as exc:
                errors.append(f"final tc snapshot: {redact(str(exc))}")
            try:
                self.tc(["qdisc", "del", "dev", "lo", "root"])
                self.report["qdisc_removed"] = True
                remaining = self.tc_json(["-j", "qdisc", "show", "dev", "lo"])
                if any(q.get("kind") != "noqueue" for q in remaining):
                    raise RuntimeError("unexpected qdisc remains after cleanup")
            except Exception as exc:
                errors.append(f"qdisc cleanup: {redact(str(exc))}")
        return errors


def arguments(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Manual loopback-only netem acceptance; run as root in a fresh unshare -n namespace. Python 3.10+ required. All app and diagnostic workers run as the non-root actor.")
    parser.add_argument("--renetd-bin", required=True, help="Path to a built renetd executable")
    parser.add_argument("--fakezgw-bin", required=True, help="Path to a built fakezgw executable")
    parser.add_argument("--output", required=True, help="New private output directory; its parent must already exist. Contains report.json plus private logs/state/invitation.")
    parser.add_argument("--actor-uid", required=True, type=int, help="Non-root UID for every application and diagnostic subprocess")
    parser.add_argument("--actor-gid", required=True, type=int, help="Non-root primary GID; supplementary groups are cleared")
    parser.add_argument("--ip-bin", default="/usr/bin/ip", help="Trusted ip executable path (default: /usr/bin/ip)")
    parser.add_argument("--tc-bin", default="/sbin/tc", help="Trusted tc executable path (default: /sbin/tc)")
    return parser.parse_args(argv)


def interrupted(signum: int, frame: FrameType | None) -> None:
    raise InterruptedError(f"interrupted by signal {signum}")


def main(argv: list[str]) -> int:
    if argv == ["--_tool-worker"]:
        return tool_worker()
    args = arguments(argv)
    report: dict[str, Any] = {
        "schema_version": 1, "ok": False, "stage": "preflight", "parameters": vars(args).copy(),
        "fixture": {"vin": VIN, "request_hex": REQUEST.hex(), "ack_hex": ACK.hex(), "response_hex": RESPONSE.hex(),
                    "hub": HUB, "fake": "127.0.0.2:16801/16811", "presenter": "127.0.0.1:26801/26811"},
        "profiles": {"baseline": {"one_way_delay_ms": 0, "configured_loss_percent": 0},
                     "degraded": {"one_way_delay_ms": 100, "nominal_rtt_ms": 200, "configured_loss_percent": 1},
                     "blackhole": {"one_way_delay_ms": 100, "configured_loss_percent": 100}},
        "measurement_definition": "Milliseconds from sendall start through complete raw ACK and VIN response on one persistent diagnostic TCP connection; recovery uses a new TCP connection. No latency performance assertion.",
        "kernel_counter_note": "tc counters are observed packets/bytes/drops, not a measured 1% loss ratio. Degraded zero drops are possible; blackhole must show actual kernel drops. Both TCP directions on lo egress traverse netem once each.",
        "replay_scope": "Network recovery starts only after the client reports disconnection and the old vehicle socket is closed. The assertion of no replay covers only the new generation; it cannot guarantee that an already sent request was never executed on the old TCP connection when connectivity returns earlier.",
        "replay_observation_seconds": REPLAY_OBSERVATION, "blackhole_observation_seconds": BLACKHOLE_OBSERVATION,
        "measurements_ms": {name: [] for name in ("baseline", "degraded", "recovery")},
        "successful_requests": {name: 0 for name in ("baseline", "degraded", "recovery")},
        "queued_request_not_delivered_at_blackhole_observation": None, "queued_not_replayed": None,
        "old_tool_connection": {"eof": None}, "blackhole_kernel_drops": None, "final_fake_counts": None,
        "tc_commands": [], "tc_snapshots": [], "peer_only_tests": [], "qdisc_removed": False,
    }
    harness = Harness(args, report)
    output_ready = False
    old_umask = os.umask(0o077)
    try:
        if args.actor_uid <= 0 or args.actor_gid <= 0:
            raise ValueError("actor UID and GID must both be non-root positive IDs")
        for name in ("renetd_bin", "fakezgw_bin", "ip_bin", "tc_bin"):
            path = Path(getattr(args, name)).resolve(strict=True)
            if not path.is_file() or not os.access(path, os.X_OK):
                raise ValueError(f"{name} must be an executable file")
            setattr(args, name, str(path))
        harness.prepare_output()
        output_ready = True
        signal.signal(signal.SIGINT, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        harness.exercise()
        report["ok"] = True
    except (Exception, KeyboardInterrupt) as exc:
        report["error"] = redact(f"{type(exc).__name__}: {exc}")
    finally:
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        cleanup_errors = harness.cleanup()
        if cleanup_errors:
            report["cleanup_errors"] = cleanup_errors
            report["ok"] = False
        report["elapsed_s"] = time.monotonic() - harness.started
        report["successful_requests_total"] = sum(report["successful_requests"].values())
        report["latency_summary_ms"] = {
            name: {"count": len(samples), "min": min(samples) if samples else None,
                   "p50": percentile(samples, 0.50), "p95": percentile(samples, 0.95),
                   "max": max(samples) if samples else None, "method": "linear interpolation"}
            for name, samples in report["measurements_ms"].items()
        }
        encoded = json.dumps(report, indent=2, sort_keys=True) + "\n"
        try:
            if not output_ready:
                raise RuntimeError("output directory was not created")
            fd, temporary = tempfile.mkstemp(prefix=".report-", dir=harness.output)
            with os.fdopen(fd, "w", encoding="utf-8") as output:
                os.fchown(output.fileno(), args.actor_uid, args.actor_gid)
                output.write(encoded)
            os.replace(temporary, harness.output / "report.json")
            print(f"report: {harness.output / 'report.json'}")
        except Exception:
            report["ok"] = False
            print(json.dumps(report, indent=2, sort_keys=True))
        os.umask(old_umask)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
