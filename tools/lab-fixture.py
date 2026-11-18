#!/usr/bin/env python3
"""Explicit root-only, disposable network-namespace fixture; never launches Client."""
from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import pwd
import secrets
import shlex
import selectors
import shutil
import signal
import socket
import subprocess
import struct
import stat
import sys
import time
import threading
from typing import Any

FORCE_COMMAND = r"""
import hashlib
import json
import os
import signal
import sys

signal.alarm(30)
command = os.environ.get("SSH_ORIGINAL_COMMAND", "")
sizes = {"1048576": 1048576, "8388608": 8388608}
if command in ("", "read-only-marker"):
    sys.stdout.write("remote-enet-mgu-fixture")
elif command == "read-only-bulk" or command in tuple("read-only-bulk " + n for n in sizes):
    count = 1048576 if command == "read-only-bulk" else sizes[command.split(" ")[1]]
    block = bytes(65536)
    for offset in range(0, count, len(block)):
        sys.stdout.buffer.write(block[:min(len(block), count - offset)])
elif command in tuple("read-only-upload " + n for n in sizes):
    count = sizes[command.split(" ")[1]]
    remaining = count
    digest = hashlib.sha256()
    while remaining:
        block = sys.stdin.buffer.read(min(65536, remaining))
        if not block or any(block):
            sys.exit(2)
        remaining -= len(block)
        digest.update(block)
    if sys.stdin.buffer.read(1):
        sys.exit(2)
    sys.stdout.write(json.dumps({"bytes": count, "sha256": digest.hexdigest()}))
else:
    sys.exit(2)
sys.stdout.flush()
"""

DISCOVERY_PROGRAM = r"""
import signal
import socket
import struct
import stat
import sys
import time
import threading

signal.alarm(int(sys.argv[2]) + 1)
listener = socket.socket(fileno=int(sys.argv[1]))
listener.settimeout(0.2)
deadline = time.monotonic() + int(sys.argv[2])
body = b"DIAGADR10BMWMACAABBCCDDEEFFBMWVINWBAXXXXXXX0000001"
response = struct.pack("!IH", len(body), 0x11) + body
source = struct.pack("=I4s4s", 0, socket.inet_aton("169.254.91.2"), bytes(4))
with listener:
    while time.monotonic() < deadline:
        try:
            payload, peer = listener.recvfrom(7)
        except socket.timeout:
            continue
        if peer[0] == "169.254.91.1" and payload == bytes.fromhex("000000000011"):
            if listener.sendmsg([response], [(socket.IPPROTO_IP, 8, source)], 0, peer) != 56:
                sys.exit(2)
"""

SYSTEM_DIRS = ("/usr/sbin", "/usr/bin", "/sbin", "/bin")
SYSTEM_ENV = {"PATH": ":".join(SYSTEM_DIRS), "LC_ALL": "C"}
SYSTEM_NAMES = {"ip", "nsenter", "unshare", "ssh-keygen", "ss", "setpriv", "sshd", "python3", "echo"}
SERVICE_LOG_LIMIT = 8 << 20


def trusted_path(path: Path) -> str:
    if not path.is_absolute():
        raise ValueError("system executable must be absolute")
    resolved = path.resolve(strict=True)
    for component in {path, *path.parents, resolved, *resolved.parents}:
        info = component.stat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise RuntimeError("untrusted system executable path")
    info = resolved.stat()
    if not stat.S_ISREG(info.st_mode) or not info.st_mode & 0o111:
        raise RuntimeError("system executable must be a regular executable file")
    return str(resolved)


def trusted_tool(name: str) -> str:
    if name not in SYSTEM_NAMES:
        raise ValueError("unsupported system command")
    for directory in SYSTEM_DIRS:
        path = Path(directory) / name
        if path.is_file() and os.access(path, os.X_OK):
            return trusted_path(path)
    raise RuntimeError("missing system executable: " + name)


def system_command(command: list[str]) -> list[str]:
    argv = list(command)
    name = Path(argv[0]).name
    argv[0] = trusted_tool(name)
    if name in ("unshare", "nsenter", "setpriv") and "--" in argv:
        position = argv.index("--") + 1
        nested = argv[position]
        if nested == sys.executable:
            argv[position] = trusted_path(Path(nested))
        elif not Path(nested).is_absolute():
            argv[position] = trusted_tool(nested)
        # Absolute actor binaries remain actor-owned; setpriv executes them after dropping uid.
    return argv

NET = ipaddress.ip_network("169.254.91.0/29")
STOP = False


def signal_stop(signum: int, frame: Any) -> None:
    global STOP
    STOP = True


class Cancelled(Exception):
    pass


def debug_error(output: Path, stage: str, error: BaseException) -> None:
    # Diagnostics stay private, bounded and separate from portable reports.
    message = str(error).replace("\n", " ").replace("\r", " ")[:2048]
    if any(word in message.lower() for word in ("private key", "secret", "password", "invitation")):
        message = "sensitive diagnostic omitted"
    event = json.dumps({"stage": stage, "category": type(error).__name__, "message": message}) + "\n"
    try:
        descriptor = os.open(output / "debug.log", os.O_WRONLY | os.O_APPEND | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            if os.fstat(descriptor).st_size + len(event.encode()) <= 65536:
                os.write(descriptor, event.encode())
        finally:
            os.close(descriptor)
    except OSError:
        # A diagnostic write must never prevent resource cleanup or the report.
        pass


def run(argv: list[str], timeout: float = 10) -> str:
    deadline = time.monotonic() + timeout
    output = bytearray()
    with subprocess.Popen(system_command(argv), env=SYSTEM_ENV, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT) as process:
        assert process.stdout is not None
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise RuntimeError("command timeout")
                    for key, _ in selector.select(min(remaining, 0.2)):
                        block = os.read(key.fd, 65536)
                        if not block:
                            selector.unregister(key.fileobj)
                        else:
                            output.extend(block)
                            if len(output) > 1048576:
                                raise RuntimeError("command output limit")
                process.wait(timeout=max(0.001, deadline - time.monotonic()))
            if process.returncode:
                raise RuntimeError(f"{Path(argv[0]).name} exit {process.returncode}: {output.decode('utf-8', errors='replace')[:2048]}")
        except BaseException:
            process.kill()
            process.wait(timeout=5)
            raise
    return output.decode("utf-8")


def write_json(path: Path, value: Any) -> None:
    # Exclusive creation prevents following a replaced report/handshake symlink.
    with path.open("x", encoding="utf-8") as handle:
        json.dump(value, handle, indent=2)
        handle.write("\n")
    path.chmod(0o600)


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def links() -> list[dict[str, Any]]:
    return json.loads(run(["ip", "-j", "-d", "link", "show"]))


def conflicting_networks(addresses: list[dict[str, Any]], routes: list[dict[str, Any]]) -> list[str]:
    found: list[str] = []
    for interface in addresses:
        for address in interface.get("addr_info", []):
            if address.get("family") == "inet":
                network = ipaddress.ip_network(f"{address['local']}/{address['prefixlen']}", strict=False)
                if network.overlaps(NET):
                    found.append(str(network))
    for route in routes:
        destination = route.get("dst", "default")
        if destination == "default":
            continue
        network = ipaddress.ip_network(destination, strict=False)
        if network.version == 4 and network.overlaps(NET):
            found.append(str(network))
    return found


def snapshot_conflicts() -> list[str]:
    return conflicting_networks(json.loads(run(["ip", "-j", "-4", "address", "show"])),
                                json.loads(run(["ip", "-j", "-4", "route", "show", "table", "all"])))


def wait_file(path: Path, processes: list[subprocess.Popen[bytes]], deadline: float) -> Any:
    while not STOP and time.monotonic() < deadline:
        if path.exists():
            try:
                return read_json(path)
            except json.JSONDecodeError:
                pass
        if any(process.poll() is not None for process in processes):
            raise RuntimeError(f"process exited before handshake {path.name}")
        time.sleep(0.05)
    if STOP:
        raise Cancelled("startup cancelled")
    raise RuntimeError(f"timed out waiting for {path.name}")


def _stop_process(process: subprocess.Popen[bytes], group: bool = False) -> dict[str, Any]:
    forced = False
    # Every service has its own session, so only this fixture's descendants get signals.
    if group and process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGINT)
        except ProcessLookupError:
            pass
    elif not group and process.poll() is None:
        process.send_signal(signal.SIGINT)
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        forced = True
        if group:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        else:
            process.kill()
        process.wait(timeout=5)
    return {"pid": process.pid, "returncode": process.returncode, "forced": forced,
            "exit_kind": "signal" if process.returncode < 0 else "normal"}


def namespace_matches(path: Path, expected: str) -> bool:
    try:
        return os.readlink(path) == expected
    except (FileNotFoundError, ProcessLookupError):
        return False


def reclaim_namespace_processes(namespace: str) -> list[dict[str, Any]]:
    results: list[dict[str, Any]] = []
    limit = time.monotonic() + 5
    while time.monotonic() < limit:
        live: list[int] = []
        # Only the unique namespace created for this run is eligible.
        for entry in Path("/proc").iterdir():
            if not entry.name.isdecimal():
                continue
            descriptor: int | None = None
            try:
                descriptor = os.pidfd_open(int(entry.name))
                if os.readlink(entry / "ns/net") != namespace:
                    continue
                live.append(int(entry.name))
                signal.pidfd_send_signal(descriptor, signal.SIGKILL)
                results.append({"pid": int(entry.name), "forced": True})
            except (FileNotFoundError, ProcessLookupError):
                pass
            finally:
                if descriptor is not None:
                    os.close(descriptor)
        if not live:
            return results
        time.sleep(0.05)
    raise RuntimeError(f"namespace processes did not exit: {live}")


def ssh_path(path: Path) -> str:
    value = str(path)
    if any(character in value for character in '\n\r"\\'):
        raise ValueError("unsupported characters in output path")
    return f'"{value}"'


class ServiceLog:
    def __init__(self, stream: Any, descriptor: int) -> None:
        self.stream = stream
        self.descriptor = descriptor
        self.stop = threading.Event()
        self.report: dict[str, Any] = {"started": False, "joined": None, "written_bytes": 0,
                                      "dropped_bytes": 0, "truncated": False, "eof": False}
        # A failed filesystem write must not pin process exit after failed join is reported.
        self.worker = threading.Thread(target=self.drain, name="fixture-log", daemon=True)

    def consume(self, block: bytes) -> None:
        available = max(0, SERVICE_LOG_LIMIT - self.report["written_bytes"])
        retained = block[:available]
        while retained:
            written = os.write(self.descriptor, retained)
            if written <= 0:
                raise RuntimeError("short service log write")
            self.report["written_bytes"] += written
            retained = retained[written:]
        dropped = max(0, len(block) - available)
        self.report["dropped_bytes"] += dropped
        self.report["truncated"] = self.report["dropped_bytes"] > 0

    def drain(self) -> None:
        try:
            os.set_blocking(self.stream.fileno(), False)
            with selectors.DefaultSelector() as selector:
                selector.register(self.stream, selectors.EVENT_READ)
                while not self.stop.is_set():
                    for key, _ in selector.select(0.2):
                        try:
                            block = os.read(key.fd, 65536)
                        except BlockingIOError:
                            continue
                        if not block:
                            self.report["eof"] = True
                            return
                        self.consume(block)
        except Exception:
            self.report["error"] = "log-drain-failed"
        finally:
            self.stream.close()
            os.close(self.descriptor)

    def start(self) -> None:
        self.worker.start()
        self.report["started"] = True

    def finish(self) -> dict[str, Any]:
        if self.report["started"]:
            # A descendant may retain stdout forever. Stop polling without waiting for EOF.
            self.stop.set()
            self.worker.join(timeout=2)
            self.report["joined"] = not self.worker.is_alive()
        return dict(self.report)


def finish_service_log(process: subprocess.Popen[bytes]) -> dict[str, Any] | None:
    capture = getattr(process, "fixture_log", None)
    if not isinstance(capture, ServiceLog):
        return None
    result = capture.finish()
    if result.get("error") or result["joined"] is False:
        raise RuntimeError("service log worker failed to join or drain")
    return result


def stop_process(process: subprocess.Popen[bytes], group: bool = False) -> dict[str, Any]:
    try:
        result = _stop_process(process, group)
    finally:
        log = finish_service_log(process)
    if log is not None:
        result["log"] = log
    return result


def start_service(argv: list[str], log: Path) -> subprocess.Popen[bytes]:
    descriptor = os.open(log, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600)
    process: subprocess.Popen[bytes] | None = None
    try:
        process = subprocess.Popen(system_command(argv), env=SYSTEM_ENV, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        assert process.stdout is not None
        capture = ServiceLog(process.stdout, descriptor)
        process.fixture_log = capture
        capture.start()
        return process
    except BaseException:
        if process is not None:
            process.kill()
            process.wait(timeout=5)
            if process.stdout is not None:
                process.stdout.close()
        os.close(descriptor)
        raise


def identification() -> bytes:
    body = b"DIAGADR10BMWMACAABBCCDDEEFFBMWVINWBAXXXXXXX0000001"
    return struct.pack("!IH", len(body), 0x11) + body


def probe_discovery(deadline: float) -> dict[str, Any]:
    # Parent binds only an ephemeral source port, never host port 6811.
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
        probe.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
        probe.bind(("169.254.91.1", 0))
        for _ in range(10):
            if STOP or time.monotonic() >= deadline:
                break
            probe.sendto(bytes.fromhex("000000000011"), ("169.254.91.7", 6811))
            probe.settimeout(max(0.001, min(0.3, deadline - time.monotonic())))
            try:
                payload, peer = probe.recvfrom(57)
            except socket.timeout:
                continue
            if peer != ("169.254.91.2", 6811) or payload != identification():
                raise RuntimeError(f"unexpected discovery response source/ident: {peer}")
            return {"verified": True, "source": list(peer), "length": len(payload),
                    "destination": "169.254.91.7:6811"}
    raise RuntimeError("broadcast discovery reply source/ident verification failed")


def child(args: argparse.Namespace) -> int:
    if os.geteuid() != 0 or not args.parent_netns or not args.token:
        raise RuntimeError("child requires root and private parent handshake")
    output = Path(args.output)
    processes: list[subprocess.Popen[bytes]] = []
    discovery: subprocess.Popen[bytes] | None = None
    report: dict[str, Any] = {"stage": "namespace-check", "processes": []}
    deadline = time.monotonic() + args.lifetime_seconds
    try:
        namespace = os.readlink("/proc/self/ns/net")
        if namespace == args.parent_netns or [entry["ifname"] for entry in links()] != ["lo"]:
            raise RuntimeError("child must start in a new network namespace containing only lo")
        write_json(output / "child-hello.json", {"pid": os.getpid(), "netns": namespace})
        report["stage"] = "wait-network"
        permit = wait_file(output / "network-ready.json", [], min(deadline, time.monotonic() + 30))
        if permit.get("token") != args.token:
            raise RuntimeError("invalid private network handshake")
        report["stage"] = "start-services"
        listener = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            listener.setsockopt(socket.IPPROTO_IP, getattr(socket, "IP_PKTINFO", 8), 1)
            listener.settimeout(0.2)
            listener.bind(("0.0.0.0", 6811))
            report["discovery"] = {"bind": list(listener.getsockname()), "source": "169.254.91.2:6811",
                                   "worker_uid": args.actor_uid}
            discovery = subprocess.Popen(
                system_command(["setpriv", "--reuid", str(args.actor_uid), "--regid", str(args.actor_gid),
                 "--clear-groups", "--no-new-privs", "--", sys.executable, "-I", "-c",
                 DISCOVERY_PROGRAM, str(listener.fileno()), str(args.lifetime_seconds)]), env=SYSTEM_ENV,
                pass_fds=(listener.fileno(),), stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            processes.append(discovery)
            listener.close()
        except Exception:
            listener.close()
            raise
        fake = start_service(["setpriv", "--reuid", str(args.actor_uid), "--regid", str(args.actor_gid), "--clear-groups", "--no-new-privs", "--", args.fakezgw_bin, "--bind", "169.254.91.2", "--alive", "0",
                              "--vin", "WBAXXXXXXX0000001", "--disc-port", "16811", "--multi-responders",
                              str(Path(args.actor_directory) / "responders.json")], output / "fakezgw.log")
        processes.append(fake)
        sshd = start_service(["/usr/sbin/sshd", "-D", "-e", "-f", str(output / "sshd_config")], output / "sshd.log")
        processes.append(sshd)
        # Probe all listeners within this namespace; no host/public connections.
        for host, port in [("169.254.91.2", 6801), ("169.254.91.2", 6811), ("169.254.91.3", 22)]:
            limit = min(deadline, time.monotonic() + 10)
            while True:
                if STOP:
                    raise Cancelled("startup cancelled")
                if any(p.poll() is not None for p in processes):
                    raise RuntimeError("service exited during startup")
                try:
                    with socket.create_connection((host, port), timeout=0.3):
                        break
                except OSError:
                    if time.monotonic() >= limit:
                        raise RuntimeError(f"listener not ready: {host}:{port}") from None
                    time.sleep(0.05)
        udp = run(["ss", "-H", "-lun"])
        endpoints = {line.split()[3] for line in udp.splitlines() if len(line.split()) >= 4}
        if not {"0.0.0.0:6811", "169.254.91.2:16811"}.issubset(endpoints):
            raise RuntimeError("wildcard discovery/fakezgw UDP listeners not ready")
        if discovery is None or discovery.poll() is not None:
            raise RuntimeError("discovery worker failed during startup")
        write_json(output / "child-ready.json", {"pid": os.getpid(), "netns": namespace,
                                                  "service_pids": [p.pid for p in processes],
                                                  "discovery_bind": report["discovery"]["bind"]})
        report["stage"] = "serving"
        while not STOP and time.monotonic() < deadline:
            if any(p.poll() is not None for p in processes):
                raise RuntimeError("fixture service exited")
            if discovery is None or discovery.poll() is not None:
                raise RuntimeError("discovery worker exited unexpectedly")
            time.sleep(0.2)
    except Cancelled:
        report["outcome"] = "cancelled"
    except Exception as error:
        report["error"] = "startup-failed" if report["stage"] != "serving" else "service-failed"
        debug_error(output, report["stage"], error)
    finally:
        for process in reversed(processes):
            try:
                report["processes"].append(stop_process(process, group=True))
            except Exception as error:
                report.setdefault("cleanup_errors", []).append("process-reap-failed")
                debug_error(output, "child-cleanup", error)
        discovery_status = report.setdefault("discovery", {})
        discovery_status["started"] = discovery is not None
        discovery_status["joined"] = None if discovery is None else discovery.poll() is not None
        discovery_status["state"] = "not-started" if discovery is None else "reaped" if discovery.poll() is not None else "reap-failed"
        if discovery is not None and discovery.poll() is None:
            report.setdefault("cleanup_errors", []).append("discovery-reap-failed")
        report.setdefault("outcome", "failed" if "error" in report else "cancelled" if STOP else "completed")
        write_json(output / "child-report.json", report)
    return 1 if "error" in report or report.get("cleanup_errors") else 0


def validate_authorization_command() -> None:
    executable = Path("/usr/bin/echo")
    resolved = executable.resolve(strict=True)
    for entry in {executable, *executable.parents, resolved, *resolved.parents}:
        status = entry.stat()
        if status.st_uid != 0 or status.st_mode & 0o022:
            raise RuntimeError("authorization command path must be root-owned and non-writable")
    status = resolved.stat()
    if not stat.S_ISREG(status.st_mode) or not status.st_mode & 0o111:
        raise RuntimeError("authorization command must be an executable regular file")


def sshd_config(output: Path, username: str, public_key: str) -> str:
    fields = public_key.split()
    if len(fields) < 2 or fields[0] != "ssh-ed25519":
        raise ValueError("fixture requires an ed25519 public key")
    try:
        decoded = base64.b64decode(fields[1], validate=True)
    except (ValueError, binascii.Error) as error:
        raise ValueError("invalid fixture public key encoding") from error
    prefix = struct.pack("!I", 11) + b"ssh-ed25519" + struct.pack("!I", 32)
    if len(decoded) != len(prefix) + 32 or not decoded.startswith(prefix):
        raise ValueError("invalid fixture ed25519 public key")
    if base64.b64encode(decoded).decode("ascii") != fields[1]:
        raise ValueError("noncanonical fixture public key")
    # Only validated algorithm/base64 are passed; no user-provided substitutions.
    authorization = 'AuthorizedKeysCommand /usr/bin/echo "ssh-ed25519" "' + fields[1] + '"'
    return "\n".join([
        "Port 22", "ListenAddress 169.254.91.3", "AddressFamily inet",
        "HostKey " + ssh_path(output / "hostkey"), "PidFile " + ssh_path(output / "sshd.pid"),
        "AuthorizedKeysFile none", authorization, "AuthorizedKeysCommandUser " + username,
        "AllowUsers " + username,
        "AuthenticationMethods publickey", "PubkeyAuthentication yes", "PasswordAuthentication no",
        "KbdInteractiveAuthentication no", "UsePAM no", "StrictModes yes", "PermitRootLogin no",
        "DisableForwarding yes", "AllowAgentForwarding no", "AllowTcpForwarding no", "X11Forwarding no",
        "PermitTunnel no", "PermitTTY no", "PermitUserRC no", "PermitUserEnvironment no",
        "ForceCommand " + trusted_tool("python3") + " -I -c " + shlex.quote("exec(" + repr(FORCE_COMMAND) + ")"),
        "LogLevel VERBOSE", "MaxAuthTries 3", "LoginGraceTime 15", "MaxSessions 2",
        "PrintMotd no", "PrintLastLog no", "",
    ])


def export_metadata(username: str, host_public_key: str) -> dict[str, Any]:
    return {
        "schema_version": 1, "gateway": "169.254.91.2:6801",
        "target": "169.254.91.3", "ssh_user": username,
        "identity_file": "userkey", "host_key_file": "hostkey.pub",
        "host_public_key": host_public_key.strip(),
    }


def owned_link(entries: list[dict[str, Any]], name: str, alias: str) -> bool:
    matches = [entry for entry in entries if entry["ifname"] == name]
    if not matches:
        return False
    if len(matches) != 1 or matches[0].get("ifalias") != alias:
        raise RuntimeError("interface ownership changed; refusing deletion")
    return True


def configure_network(process: subprocess.Popen[bytes], child_netfd: int,
                      child_namespace: str, parent_namespace: str, host: str,
                      peer: str, alias: str, report: dict[str, Any]) -> None:
    enter = ["nsenter", "--net=" + f"/proc/{os.getpid()}/fd/{child_netfd}", "--", "ip"]

    def identity() -> None:
        if STOP:
            raise Cancelled("network startup cancelled")
        if os.readlink("/proc/self/ns/net") != parent_namespace:
            raise RuntimeError("parent namespace identity changed")
        if process.poll() is not None or os.readlink(f"/proc/{process.pid}/ns/net") != child_namespace:
            raise RuntimeError("child namespace identity changed")
        if f"net:[{os.fstat(child_netfd).st_ino}]" != child_namespace:
            raise RuntimeError("pinned child namespace identity changed")

    def step(name: str, command: list[str]) -> None:
        identity()
        report["stage"] = name
        report.setdefault("network_steps", []).append({"step": name, "completed": False})
        run(command)
        report["network_steps"][-1]["completed"] = True

    identity()
    if snapshot_conflicts():
        raise RuntimeError("host subnet conflict appeared before network creation")
    if any(entry["ifname"] in (host, peer) for entry in links()):
        raise RuntimeError("random host interface name collision")
    child_links = json.loads(run(enter + ["-j", "-d", "link", "show"]))
    if [entry["ifname"] for entry in child_links] != ["lo"]:
        raise RuntimeError("child namespace no longer contains only lo")
    # Creation and explicit alias writes stay in the disposable namespace.
    step("create-child-veth", enter + ["link", "add", "name", host, "type", "veth", "peer", "name", peer])
    step("mark-child-host", enter + ["link", "set", "dev", host, "alias", alias])
    step("mark-child-peer", enter + ["link", "set", "dev", peer, "alias", alias])
    identity()
    report["stage"] = "verify-child-aliases"
    entries = json.loads(run(enter + ["-j", "-d", "link", "show"]))
    if not owned_link(entries, host, alias) or not owned_link(entries, peer, alias):
        raise RuntimeError("child veth aliases not confirmed")
    if snapshot_conflicts() or any(entry["ifname"] in (host, peer) for entry in links()):
        raise RuntimeError("host collision appeared before marked veth move")
    # A timed-out move may have succeeded. Cleanup checks ownership even then.
    report["host_move_attempted"] = True
    step("move-marked-host", enter + ["link", "set", "dev", host, "netns", str(os.getpid())])
    identity()
    report["stage"] = "verify-host-alias"
    if not owned_link(links(), host, alias):
        raise RuntimeError("moved host veth alias missing")
    for name, command in (
        ("address-host", ["ip", "address", "add", "169.254.91.1/29", "dev", host]),
        ("up-host", ["ip", "link", "set", "dev", host, "up"]),
        ("up-child-loopback", enter + ["link", "set", "dev", "lo", "up"]),
        ("address-child-gateway", enter + ["address", "add", "169.254.91.2/29", "dev", peer]),
        ("address-child-mgu", enter + ["address", "add", "169.254.91.3/32", "dev", peer]),
        ("up-child-peer", enter + ["link", "set", "dev", peer, "up"]),
    ):
        step(name, command)


def actor_permission(status: os.stat_result, uid: int, gid: int, bit: int) -> bool:
    shift = 6 if status.st_uid == uid else 3 if status.st_gid == gid else 0
    return bool(status.st_mode & (bit << shift))


def validate_probe_binary(value: str, uid: int, gid: int) -> None:
    path = Path(value)
    if not path.is_absolute():
        raise ValueError("probe binary must be absolute")
    resolved = path.resolve(strict=True)
    status = resolved.stat()
    if not stat.S_ISREG(status.st_mode) or not actor_permission(status, uid, gid, 1):
        raise ValueError("probe binary must be an actor-executable regular file")
    for ancestor in {*path.parents, *resolved.parents}:
        if not actor_permission(ancestor.stat(), uid, gid, 1):
            raise ValueError("actor cannot traverse probe binary ancestor")


def validate_probe_report(value: Any, size: int) -> bool:
    required = {"schema", "version", "platform", "success", "dry_run", "cancelled", "timeout",
                "cleanup", "expected_bytes", "expected_sha256", "phases"}
    if not isinstance(value, dict) or set(value) != required or value["schema"] != "labprobe/v1":
        return False
    for field, expected in (("success", True), ("cleanup", True), ("dry_run", False),
                            ("cancelled", False), ("timeout", False)):
        if value[field] is not expected:
            return False
    if any(not isinstance(value[field], str) or not value[field] or len(value[field]) > 128 for field in ("version", "platform")):
        return False
    digest = hashlib.sha256(bytes(size)).hexdigest()
    if type(value["expected_bytes"]) is not int or value["expected_bytes"] != size or value["expected_sha256"] != digest:
        return False
    phases = value["phases"]
    stages = ["keys", "discover", "ssh", "marker", "download", "upload"]
    if not isinstance(phases, list) or len(phases) != len(stages):
        return False
    for phase, stage in zip(phases, stages):
        fields = {"stage", "code", "duration_ms", "bytes"}
        transfer = stage in ("download", "upload")
        if transfer:
            fields.add("sha256")
        if not isinstance(phase, dict) or set(phase) != fields or phase["stage"] != stage or phase["code"] != "ok":
            return False
        if type(phase["duration_ms"]) is not int or not 0 <= phase["duration_ms"] <= 120000:
            return False
        if type(phase["bytes"]) is not int or phase["bytes"] != (size if transfer else 0):
            return False
        if transfer and phase["sha256"] != digest:
            return False
    return True


def probe_command(binary: str, uid: int, gid: int, username: str, actor_directory: Path,
                  size: int, timeout: int) -> list[str]:
    if uid <= 0 or gid <= 0 or size not in (1048576, 8388608) or not 1 <= timeout <= 120:
        raise ValueError("invalid fixed probe budget or payload")
    return ["setpriv", "--reuid", str(uid), "--regid", str(gid), "--clear-groups", "--no-new-privs", "--",
            binary, "--run", "--gateway", "169.254.91.2:6801", "--target", "169.254.91.3",
            "--ssh-user", username, "--identity-file", str(actor_directory / "userkey"),
            "--host-key-file", str(actor_directory / "hostkey.pub"), "--payload-bytes", str(size),
            "--timeout", str(timeout) + "s"]


def overall_success(report: dict[str, Any], probe_required: bool) -> bool:
    return ("error" not in report and "child_error" not in report and not report.get("cleanup_errors")
            and (not probe_required or (not STOP and report.get("probe_result", {}).get("success") is True)))


def execute_probe(command: list[str], output: Path, size: int, timeout: int) -> dict[str, Any]:
    result: dict[str, Any] = {"success": False, "code": "probe-start-failed", "exitcode": None, "process_reaped": False}
    process: subprocess.Popen[bytes] | None = None
    captured = bytearray()
    try:
        if STOP:
            raise Cancelled("probe cancelled before start")
        process = subprocess.Popen(system_command(command), env=SYSTEM_ENV, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        assert process.stdout is not None
        deadline = time.monotonic() + timeout + 2
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while selector.get_map():
                if STOP:
                    raise Cancelled("probe cancelled")
                if time.monotonic() >= deadline:
                    result["code"] = "probe-timeout"
                    raise TimeoutError("probe budget exhausted")
                for key, _ in selector.select(0.2):
                    block = os.read(key.fd, 65536)
                    if not block:
                        selector.unregister(key.fileobj)
                    else:
                        captured.extend(block)
                        if len(captured) > 1048576:
                            result["code"] = "probe-output-limit"
                            raise ValueError("probe output limit")
            process.wait(timeout=2)
        result["exitcode"] = process.returncode
        result["code"] = "probe-exit-failed" if process.returncode != 0 else "probe-report-invalid"
        if process.returncode == 0:
            value = json.loads(captured)
            if validate_probe_report(value, size):
                result.update(success=True, code="ok", report=value)
    except Cancelled:
        result.update(code="probe-cancelled", cancelled=True)
    except Exception as error:
        debug_error(output, "probe", error)
    finally:
        if process is not None:
            try:
                if process.poll() is None:
                    result["termination"] = stop_process(process, group=True)
                result["exitcode"] = process.returncode
                result["process_reaped"] = process.poll() is not None
            except Exception as error:
                result.update(success=False, code="probe-reap-failed")
                debug_error(output, "probe-cleanup", error)
            if process.stdout is not None:
                process.stdout.close()
        # Raw probe output stays root-private and bounded, never copied into exports.
        try:
            descriptor = os.open(output / "probe-output.json", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(captured[:1048576])
        except OSError as error:
            result.update(success=False, code="probe-output-write-failed")
            debug_error(output, "probe-output", error)
        if not result["process_reaped"]:
            result["success"] = False
    return result


def parent(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        raise RuntimeError("root orchestration is required; this tool never elevates itself")
    if not hasattr(os, "pidfd_open") or not hasattr(signal, "pidfd_send_signal"):
        raise RuntimeError("Python/Linux pidfd support is required for race-safe cleanup")
    descriptor = os.pidfd_open(os.getpid())
    try:
        signal.pidfd_send_signal(descriptor, 0)
    finally:
        os.close(descriptor)
    for binary in ("ip", "nsenter", "unshare", "ssh-keygen", "ss", "setpriv", "sshd", "python3", "echo"):
        trusted_tool(binary)
    trusted_path(Path(sys.executable))
    if not Path("/run/sshd").is_dir():
        raise RuntimeError("/run/sshd must already exist; tool will not create or modify it")
    if not os.access("/usr/sbin/sshd", os.X_OK):
        raise RuntimeError("/usr/sbin/sshd must already be installed")
    validate_authorization_command()
    fake = Path(args.fakezgw_bin)
    if not fake.is_absolute() or not fake.is_file() or not os.access(fake, os.X_OK):
        raise RuntimeError("--fakezgw-bin must be an absolute executable file")
    actor = pwd.getpwuid(args.actor_uid)
    if actor.pw_gid != args.actor_gid or args.actor_uid <= 0 or args.actor_gid <= 0 or actor.pw_shell in ("/bin/false", "/usr/sbin/nologin"):
        raise RuntimeError("actor must be an existing non-root login account with its primary gid")
    if args.probe_bin:
        validate_probe_binary(args.probe_bin, args.actor_uid, args.actor_gid)
    if args.lifetime_seconds <= 0 or args.lifetime_seconds > 86400:
        raise RuntimeError("lifetime must be in 1..86400 seconds")
    if not actor.pw_name or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for c in actor.pw_name):
        raise RuntimeError("unsupported actor name")
    output = Path(args.output)
    if not output.is_absolute() or output.exists() or output.is_symlink():
        raise RuntimeError("--output must be an absolute new directory")
    ssh_path(output)
    for ancestor in [output.parent, *output.parent.parents]:
        status = ancestor.stat()
        if status.st_uid != 0 or (status.st_mode & 0o022 and not status.st_mode & stat.S_ISVTX):
            raise RuntimeError("output ancestors must be root-owned and immutable to actor")
        if status.st_uid == args.actor_uid:
            searchable = bool(status.st_mode & 0o100)
        elif status.st_gid == args.actor_gid:
            searchable = bool(status.st_mode & 0o010)
        else:
            searchable = bool(status.st_mode & 0o001)
        if not searchable:
            raise RuntimeError(f"actor cannot traverse output ancestor: {ancestor}")
    if snapshot_conflicts():
        raise RuntimeError("host address/non-default route conflicts with fixture subnet")
    output.mkdir(mode=0o700)
    report: dict[str, Any] = {"stage": "prepare", "root_pid": os.getpid(), "cleanup_errors": []}
    process: subprocess.Popen[bytes] | None = None
    child_pidfd: int | None = None
    child_netfd: int | None = None
    token = secrets.token_hex(16)
    suffix = secrets.token_hex(5)
    host, peer = "w11h" + suffix, "w11p" + suffix
    alias = "remote-enet-wave11:" + token
    report.update({"host_iface": host, "peer_iface": peer, "ifalias": alias})
    deadline = time.monotonic() + args.lifetime_seconds
    parent_namespace = os.readlink("/proc/self/ns/net")
    try:
        write_json(output / "responders.json", {
            "22172a": [{"source": 99, "payload": "62172a00a9fe5b03fffffff800000000"}],
            "22f190": [{"source": 99, "payload": "62f190" + b"WBAXXXXXXX0000001".hex()}]})
        for name in ["hostkey", "userkey"]:
            run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "remote-enet-wave11-fixture", "-f", str(output / name)])
        public_client = output.parent / (output.name + "-client-" + suffix)
        public_client.mkdir(mode=0o700)
        report["client_directory"] = str(public_client)
        report["credentials_retained_for_manual_review"] = True
        for source, target in [("userkey", "userkey"), ("userkey.pub", "userkey.pub"),
                               ("hostkey.pub", "hostkey.pub")]:
            shutil.copyfile(output / source, public_client / target)
            (public_client / target).chmod(0o600)
            os.chown(public_client / target, args.actor_uid, args.actor_gid)
        os.chown(public_client, args.actor_uid, args.actor_gid)
        shutil.copyfile(output / "responders.json", public_client / "responders.json")
        (public_client / "responders.json").chmod(0o644)
        os.chown(public_client / "responders.json", args.actor_uid, args.actor_gid)
        write_json(public_client / "export.json", export_metadata(
            actor.pw_name, (public_client / "hostkey.pub").read_text(encoding="ascii")))
        os.chown(public_client / "export.json", args.actor_uid, args.actor_gid)
        config = sshd_config(output, actor.pw_name, (output / "userkey.pub").read_text(encoding="ascii"))
        (output / "sshd_config").write_text(config, encoding="utf-8")
        for path in output.iterdir():
            path.chmod(0o600)
        run(["/usr/sbin/sshd", "-t", "-f", str(output / "sshd_config")])
        report["stage"] = "spawn-namespace"
        argv = ["unshare", "-n", "--", sys.executable,
                str(Path(__file__).resolve()), "--child", "--fakezgw-bin", str(fake),
                "--run", "--actor-uid", str(args.actor_uid), "--actor-gid", str(args.actor_gid), "--actor-directory", str(public_client), "--output", str(output), "--lifetime-seconds", str(args.lifetime_seconds),
                "--parent-netns", os.readlink("/proc/self/ns/net"), "--token", token]
        process = start_service(argv, output / "child.log")
        hello = wait_file(output / "child-hello.json", [process], min(deadline, time.monotonic() + 15))
        if hello["pid"] != process.pid or hello["netns"] == os.readlink("/proc/self/ns/net"):
            raise RuntimeError("unshare did not preserve verified child PID/new namespace")
        child_pidfd = os.pidfd_open(process.pid)
        child_netfd = os.open(f"/proc/{process.pid}/ns/net", os.O_RDONLY)
        actual_namespace = os.readlink(f"/proc/{process.pid}/ns/net")
        if actual_namespace != hello["netns"] or f"net:[{os.fstat(child_netfd).st_ino}]" != hello["netns"] or process.poll() is not None:
            raise RuntimeError("child namespace identity mismatch")
        report["child_pid"] = process.pid
        report["child_netns"] = hello["netns"]
        configure_network(process, child_netfd, report["child_netns"], parent_namespace,
                          host, peer, alias, report)
        write_json(output / "network-ready.json", {"token": token})
        report["network_permitted"] = True
        report["stage"] = "wait-services"
        services = wait_file(output / "child-ready.json", [process], min(deadline, time.monotonic() + 25))
        report["service_pids"] = services["service_pids"]
        if services.get("discovery_bind") != ["0.0.0.0", 6811]:
            raise RuntimeError("child discovery listener not wildcard 6811")
        report["stage"] = "probe-broadcast-source"
        report["discovery_probe"] = probe_discovery(min(deadline, time.monotonic() + 5))
        ready = {"rootPID": os.getpid(), "childPID": process.pid, "iface": host, "peer": peer,
                 "gateway": "169.254.91.2", "mgu": "169.254.91.3", "username": actor.pw_name, "discovery_probe": report["discovery_probe"],
                 "userkey": str(public_client / "userkey"), "hostpub": str(public_client / "hostkey.pub")}
        write_json(output / "ready.json", ready)
        report["fixture_ready"] = True
        print(json.dumps(ready), flush=True)
        report["stage"] = "ready"
        if args.probe_bin:
            report["stage"] = "probe"
            budget = min(120, int(deadline - time.monotonic()) - 4)
            if budget < 1:
                report["probe_result"] = {"success": False, "code": "probe-no-budget", "exitcode": None, "process_reaped": True}
            else:
                command = probe_command(args.probe_bin, args.actor_uid, args.actor_gid, actor.pw_name,
                                        public_client, args.payload_bytes, budget)
                report["probe_result"] = execute_probe(command, output, args.payload_bytes, budget)
            if not report["probe_result"]["success"]:
                report["error"] = "probe-failed"
        while not args.probe_bin and not STOP and time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("namespace child exited")
            time.sleep(0.2)
    except Cancelled:
        report["outcome"] = "cancelled"
    except Exception as error:
        report["error"] = "stage-failed"
        debug_error(output, report["stage"], error)
        print(json.dumps({"stage": report["stage"], "error": "stage-failed"}), file=sys.stderr, flush=True)
    finally:
        if process is not None:
            try:
                # Child handles its service groups before parent escalates.
                if process.poll() is None:
                    process.terminate()
                try:
                    process.wait(timeout=15)
                    report["child_exit"] = {"returncode": process.returncode, "forced": False, "exit_kind": "signal" if process.returncode < 0 else "normal"}
                    if process.returncode != 0:
                        report["child_exit"]["interrupted_before_permit"] = not report.get("network_permitted", False)
                        if report.get("network_permitted"):
                            report["child_error"] = "child-exit-failed"
                except subprocess.TimeoutExpired:
                    report["child_exit"] = stop_process(process, group=True)
                if (output / "child-report.json").exists():
                    report["child_report"] = read_json(output / "child-report.json")
                    if report["child_report"].get("error"):
                        report["child_error"] = "child-startup-or-service-failed"
                    if report["child_report"].get("cleanup_errors"):
                        report["cleanup_errors"].append("child-cleanup-failed")
                elif report.get("network_permitted"):
                    report["cleanup_errors"].append("child-report-missing")
                else:
                    report["child_report_state"] = "interrupted-before-service-permit"
            except Exception as error:
                report["cleanup_errors"].append("cleanup-failed")
                debug_error(output, "parent-cleanup", error)
        if process is not None:
            try:
                report["child_log"] = finish_service_log(process)
            except Exception as error:
                report["cleanup_errors"].append("child-log-cleanup-failed")
                debug_error(output, "child-log-cleanup", error)
        if "child_netns" in report:
            try:
                report["residual_processes"] = reclaim_namespace_processes(report["child_netns"])
                report["namespace_processes_reclaimed"] = True
            except Exception as error:
                report["cleanup_errors"].append("cleanup-failed")
                debug_error(output, "parent-cleanup", error)
        try:
            if report.get("host_move_attempted") and owned_link(links(), host, alias):
                run(["ip", "link", "delete", "dev", host])
            remaining = links()
            report["interfaces_reclaimed"] = not any(entry["ifname"] in [host, peer] for entry in remaining)
            report["routes_reclaimed"] = not snapshot_conflicts()
            if process is not None:
                report["namespace_process_reaped"] = process.poll() is not None
        except Exception as error:
            report["cleanup_errors"].append("cleanup-failed")
            debug_error(output, "interface-cleanup", error)
        if child_netfd is not None:
            os.close(child_netfd)
        if child_pidfd is not None:
            os.close(child_pidfd)
        report["namespace_reclaimed"] = not any(
            entry.name.isdecimal() and namespace_matches(entry / "ns/net", report.get("child_netns", ""))
            for entry in Path("/proc").iterdir())
        for key in ("interfaces_reclaimed", "routes_reclaimed", "namespace_reclaimed"):
            if report.get(key) is not True:
                report["cleanup_errors"].append(key + "-unconfirmed")
        report.setdefault("outcome", "failed" if "error" in report or "child_error" in report or report["cleanup_errors"] else "cancelled" if STOP else "completed")
        report["fixture_lifecycle"] = {"ready": report.get("fixture_ready", False), "cleanup": not report["cleanup_errors"], "child_ok": "child_error" not in report}
        report["success"] = overall_success(report, bool(args.probe_bin))
        write_json(output / "report.json", report)
        print(json.dumps({"report": str(output / "report.json"), "stage": report["stage"],
                          "cleanup_errors": report["cleanup_errors"]}), flush=True)
    return 0 if report["success"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fakezgw-bin", help="absolute path to an existing fakezgw executable")
    parser.add_argument("--output", help="absolute new private output directory")
    parser.add_argument("--actor-uid", type=int)
    parser.add_argument("--actor-gid", type=int)
    parser.add_argument("--lifetime-seconds", type=int, default=1800)
    parser.add_argument("--child", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--parent-netns", default="", help=argparse.SUPPRESS)
    parser.add_argument("--token", default="", help=argparse.SUPPRESS)
    parser.add_argument("--probe-bin", help="absolute actor-executable renet-labprobe; probe once then clean up")
    parser.add_argument("--payload-bytes", type=int, choices=(1048576, 8388608), default=1048576)
    parser.add_argument("--run", action="store_true", help="explicitly enable privileged fixture orchestration")
    parser.add_argument("--actor-directory", default="", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not args.run:
        print(json.dumps({"dry_run": True, "network": str(NET), "requires": ["--run", "--fakezgw-bin", "--output", "--actor-uid", "--actor-gid"]}))
        return 0
    if not args.fakezgw_bin or not args.output or args.actor_uid is None or args.actor_gid is None:
        parser.error("--run requires binary, output and explicit actor uid/gid")
    os.umask(0o077)
    signal.signal(signal.SIGINT, signal_stop)
    signal.signal(signal.SIGTERM, signal_stop)
    try:
        return child(args) if args.child else parent(args)
    except Exception:
        print("fixture error: preflight-failed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
