#!/usr/bin/env python3
"""Run bounded loopback stability lanes; default is a side-effect-free dry run."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any


REPORT_LIMIT = 16 * 1024
SAMPLE_LIMIT = 8 * 1024 * 1024
STOP = False
HASHES = {"1048576": "c196c433e60b1bdafd22bbacfabb8f7b7068f912940259ed2690df5ccedb3f8c",
          "8388608": "187ba28c9df62b0be31185ef307a2a2adcd0353dc11260ce19deb9e855287016"}


def seconds(value: str) -> int:
    match = re.fullmatch(r"([1-9][0-9]{0,4})([smh])", value)
    if match is None:
        raise argparse.ArgumentTypeError("expected an integer duration with s, m or h")
    return int(match[1]) * {"s": 1, "m": 60, "h": 3600}[match[2]]


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    data = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()
    descriptor, name = tempfile.mkstemp(prefix=".status-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def read_report(path: Path) -> dict[str, Any] | None:
    try:
        info = path.lstat()
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(info.st_mode) or info.st_size > REPORT_LIMIT:
        raise ValueError("invalid_lane_report")
    value = json.loads(path.read_bytes())
    if not isinstance(value, dict) or value.get("schema") != "stability-soak/v1":
        raise ValueError("invalid_lane_report")
    return value


def proc_start(pid: int) -> str:
    value = Path(f"/proc/{pid}/stat").read_text()
    return value[value.rindex(")") + 2:].split()[19]


def proc_resources(pid: int, expected: str) -> dict[str, int]:
    if proc_start(pid) != expected:
        raise ValueError("process_identity_changed")
    fields = Path(f"/proc/{pid}/statm").read_text().split()
    return {"rssBytes": int(fields[1]) * os.sysconf("SC_PAGE_SIZE"),
            "fds": len(list(Path(f"/proc/{pid}/fd").iterdir()))}


class Lane:
    def __init__(self, mode: str, process: subprocess.Popen[bytes], start: str) -> None:
        self.mode = mode
        self.process = process
        self.start = start
        self.baseline: dict[str, int] | None = None
        self.latest: dict[str, int] | None = None
        self.peak = {"rssBytes": 0, "fds": 0}
        self.report: dict[str, Any] | None = None

    def observe(self, directory: Path) -> None:
        self.report = read_report(directory / f"{self.mode}.json")
        if self.process.poll() is not None:
            return
        try:
            sample = proc_resources(self.process.pid, self.start)
        except FileNotFoundError:
            if self.process.poll() is not None:
                return
            raise
        self.latest = sample
        if self.report and self.report.get("baseline", {}).get("heapAfterGc", 0) > 0:
            if self.baseline is None:
                self.baseline = sample.copy()
            self.peak = {key: max(self.peak[key], sample[key]) for key in self.peak}
            if sample["rssBytes"] > self.baseline["rssBytes"] + 256 * 1024 * 1024 or sample["fds"] > self.baseline["fds"] + 32:
                raise ValueError("external_resource_budget")

    def stop(self) -> None:
        if self.process.poll() is not None:
            return
        if proc_start(self.process.pid) != self.start:
            raise ValueError("process_identity_changed")
        os.killpg(self.process.pid, signal.SIGTERM)
        try:
            self.process.wait(timeout=35)
            return
        except subprocess.TimeoutExpired:
            pass
        if self.process.poll() is None:
            if proc_start(self.process.pid) != self.start:
                raise ValueError("process_identity_changed")
            os.killpg(self.process.pid, signal.SIGKILL)
            self.process.wait(timeout=5)

    def summary(self) -> dict[str, Any]:
        return {"pid": self.process.pid, "start": self.start, "exitCode": self.process.poll(),
                "baseline": self.baseline, "latest": self.latest, "peak": self.peak, "report": self.report}


def passed(lane: Lane, duration: int) -> bool:
    report = lane.report
    return (lane.process.returncode == 0 and report is not None
            and report.get("mode") == lane.mode and report.get("state") == "passed"
            and report.get("requestedMs") == duration * 1000
            and report.get("elapsedMs", 0) >= duration * 1000
            and report.get("workersJoined") is True and report.get("noReplay") is True
            and report.get("resourcesWithinBudget") is True
            and report.get("path") == "relay"
            and report.get("payloadHashes") == HASHES
            and (lane.mode != "recovery" or all(report.get("faults", {}).get(mode, 0) > 0 for mode in ("cancel", "transport", "device")))
            and report.get("bytesEachWay", 0) >= 9 * 1024 * 1024)


def launch(mode: str, binary: Path, directory: Path, duration: int, interval: int) -> Lane:
    environment = os.environ.copy()
    environment.update({"GOMAXPROCS": "2", "RENET_MANUAL_STABILITY_SOAK": "1",
                        "RENET_STABILITY_DURATION": f"{duration}s", "RENET_STABILITY_INTERVAL": f"{interval}s",
                        "RENET_STABILITY_MODE": mode, "RENET_STABILITY_REPORT": str(directory / f"{mode}.json")})
    with (directory / f"{mode}.log").open("xb") as output:
        process = subprocess.Popen([str(binary), "-test.run=^TestManualStabilitySoak$", "-test.v",
                                    f"-test.timeout={duration + 120}s"], env=environment, stdout=output,
                                   stderr=subprocess.STDOUT, start_new_session=True)
    try:
        return Lane(mode, process, proc_start(process.pid))
    except (OSError, ValueError):
        if process.poll() is None:
            process.terminate()
        process.wait(timeout=5)
        raise


def request_stop(_signal: int, _frame: Any) -> None:
    global STOP
    STOP = True


def execute(options: argparse.Namespace) -> int:
    directory = Path(options.output)
    binary = Path(options.test_bin)
    info = binary.lstat()
    if not binary.is_absolute() or not stat.S_ISREG(info.st_mode) or not os.access(binary, os.X_OK):
        raise ValueError("invalid_test_binary")
    if not directory.is_absolute() or not directory.parent.is_dir():
        raise ValueError("invalid_output_directory")
    if sys.platform != "linux":
        raise ValueError("linux_resource_monitor_required")
    with binary.open("rb") as stream:
        checksum = hashlib.file_digest(stream, "sha256").hexdigest()
    directory.mkdir(mode=0o700)
    os.umask(0o077)
    signal.signal(signal.SIGINT, request_stop)
    signal.signal(signal.SIGTERM, request_stop)
    start = time.monotonic()
    state: dict[str, Any] = {"schema": "stability-supervisor/v1", "state": "running", "success": False,
                             "requestedSeconds": options.duration, "revision": options.revision,
                             "binarySha256": checksum, "lanes": {}, "cleanup": False,
                             "scope": "loopback TLS/session/L3 with memory devices; no native TUN or WAN"}
    lanes: list[Lane] = []
    error = ""
    try:
        for mode in ("steady", "recovery"):
            lanes.append(launch(mode, binary, directory, options.duration, options.interval))
        next_sample = 0.0
        while True:
            for lane in lanes:
                lane.observe(directory)
            state["elapsedSeconds"] = round(time.monotonic() - start, 3)
            state["lanes"] = {lane.mode: lane.summary() for lane in lanes}
            atomic_json(directory / "status.json", state)
            if all(lane.process.poll() is not None for lane in lanes):
                break
            if any(lane.process.poll() is not None and not passed(lane, options.duration) for lane in lanes):
                raise ValueError("lane_failed")
            if STOP:
                raise ValueError("canceled")
            if time.monotonic() - start > options.duration + 120:
                raise ValueError("supervisor_timeout")
            if time.monotonic() >= next_sample:
                row = {"elapsedSeconds": state["elapsedSeconds"], "lanes": {lane.mode: lane.latest for lane in lanes}}
                sample_path = directory / "resources.jsonl"
                if sample_path.exists() and sample_path.stat().st_size > SAMPLE_LIMIT:
                    raise ValueError("sample_budget")
                with sample_path.open("a", encoding="utf-8") as output:
                    output.write(json.dumps(row) + "\n")
                next_sample = time.monotonic() + 30
            time.sleep(1)
        if not all(passed(lane, options.duration) for lane in lanes):
            raise ValueError("incomplete_lane_evidence")
        state["state"] = "passed"
        state["success"] = True
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        error = str(exc) if isinstance(exc, ValueError) else "supervisor_error"
        state["state"] = "canceled" if STOP else "failed"
    finally:
        cleanup_errors = []
        for lane in lanes:
            try:
                lane.stop()
                lane.observe(directory)
            except (OSError, ValueError, subprocess.SubprocessError):
                cleanup_errors.append("child_cleanup_failed")
        state["cleanup"] = not cleanup_errors and all(lane.process.poll() is not None for lane in lanes)
        if not state["cleanup"]:
            state["success"] = False
            state["state"] = "failed"
        state["error"] = error
        state["cleanupErrors"] = cleanup_errors
        state["elapsedSeconds"] = round(time.monotonic() - start, 3)
        state["lanes"] = {lane.mode: lane.summary() for lane in lanes}
        atomic_json(directory / "status.json", state)
    print(json.dumps({"state": state["state"], "success": state["success"], "cleanup": state["cleanup"]}))
    return 0 if state["success"] else 1


def main(arguments: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true")
    parser.add_argument("--test-bin")
    parser.add_argument("--output")
    parser.add_argument("--revision", default="unknown")
    parser.add_argument("--duration", type=seconds, default=8 * 3600)
    parser.add_argument("--interval", type=seconds, default=30)
    options = parser.parse_args(arguments)
    if not 60 <= options.duration <= 8 * 3600 or not 1 <= options.interval <= 60:
        parser.error("duration must be 1m..8h and interval 1s..1m")
    if not options.run:
        print(json.dumps({"dryRun": True, "durationSeconds": options.duration, "lanes": ["steady", "recovery"]}))
        return 0
    if not options.test_bin or not options.output or not re.fullmatch(r"[0-9a-f]{7,40}(?:-worktree)?", options.revision):
        parser.error("--run requires --test-bin, --output and a source revision")
    try:
        return execute(options)
    except (OSError, ValueError) as exc:
        print(json.dumps({"success": False, "error": str(exc) if isinstance(exc, ValueError) else "preflight_failed"}))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
