from __future__ import annotations

import argparse
from collections.abc import Callable, Iterator
import contextlib
import copy
import importlib.util
import io
import json
import signal
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch


SPEC = importlib.util.spec_from_file_location("l3_netem_check", Path(__file__).with_name("l3-netem-check.py"))
assert SPEC is not None and SPEC.loader is not None
CHECK = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = CHECK
SPEC.loader.exec_module(CHECK)


def arguments() -> argparse.Namespace:
    return argparse.Namespace(run=True, test_binary="/tmp/integration.test", output="/tmp/new-l3-result",
                              actor_uid=1000, actor_gid=1000, profile="all")


def root_qdisc() -> dict[str, object]:
    return {"kind": "prio", "handle": "1:", "root": True,
            "options": {"bands": 3, "priomap": [0] * 16}}


def netem(packets: int = 10, drops: int = 1) -> dict[str, object]:
    return {"kind": "netem", "handle": "30:", "parent": "1:3", "bytes": packets * 100,
            "packets": packets, "drops": drops, "overlimits": 0, "requeues": 0, "backlog": 0, "qlen": 0, "options": {"limit": 10000}}


def noqueue() -> list[dict[str, object]]:
    return [{"kind": "noqueue", "handle": "0:", "root": True}]


def links(up: bool = False) -> list[dict[str, object]]:
    return [{"ifname": "lo", "link_type": "loopback", "ifindex": 1, "flags": ["LOOPBACK", *(["UP"] if up else [])]}]


def receipt() -> dict[str, object]:
    return {"schema": 1, "bytes": 1 << 20, "sha256": "a" * 64, "expected_sha256": "a" * 64,
            "active_after": 0, "workers_joined": True, "uid": 1000, "gid": 1000, "path": "relay"}


def test_output(result: dict[str, object] | None = None) -> str:
    first = receipt() if result is None else result
    second = receipt() | {"bytes": 8 << 20 if first.get("bytes") == 1 << 20 else 1 << 20}
    return ("=== RUN   TestL3ExternalNetem\n    l3_fault_test.go:317: " + CHECK.TAG
            + json.dumps(first) + "\n    l3_fault_test.go:317: " + CHECK.TAG + json.dumps(second)
            + "\n--- PASS: TestL3ExternalNetem (1.00s)\nPASS\n")


def harness() -> CHECK.Harness:
    result = CHECK.Harness(arguments(), {"commands": [], "profiles": []})
    result.tools = {name: "/usr/bin/" + name for name in ("ip", "tc", "setpriv")}
    return result


@contextlib.contextmanager
def simulated_kernel(h: CHECK.Harness, failure: str | None = None, apply: bool = True) -> Iterator[dict[str, object]]:
    state = {"up": False, "qdiscs": [], "mutations": [], "injected": False}
    h.original_up = False

    def change(operation: str, action: Callable[[], None]) -> None:
        state["mutations"].append(operation)
        failing = operation == failure and not state["injected"]
        if not failing or apply:
            action()
        if failing:
            state["injected"] = True
            raise CHECK.CheckError("process_timeout")

    def lo(up: bool) -> None:
        def action() -> None:
            state["up"] = up
            if up and state["qdiscs"] == []:
                state["qdiscs"] = noqueue()
        change("lo-up" if up else "lo-down", action)

    def tc(args: list[str]) -> None:
        if args[:2] == ["qdisc", "add"]:
            if "root" in args:
                change("root-add", lambda: state.update(qdiscs=[root_qdisc()]))
            else:
                change("netem-add", lambda: state["qdiscs"].append(netem()))
        elif args[:2] == ["filter", "add"]:
            change("filter-add", lambda: None)
        elif args[:2] == ["qdisc", "change"]:
            def action() -> None:
                state["qdiscs"][1]["options"].update(delay=args[args.index("delay") + 1], loss=args[args.index("loss") + 1])
            change("netem-change", action)
        elif args[:2] == ["qdisc", "del"]:
            change("root-delete", lambda: state.update(qdiscs=noqueue()))
        else:
            raise AssertionError(args)

    with patch.object(h, "guard", side_effect=lambda: links(state["up"])[0]), \
         patch.object(h, "qdiscs", side_effect=lambda: copy.deepcopy(state["qdiscs"])), \
         patch.object(h, "set_lo", side_effect=lo), patch.object(h, "tc", side_effect=tc):
        yield state


class PureValidationTests(unittest.TestCase):
    def test_links_reject_any_other_device_or_missing_identity(self) -> None:
        self.assertEqual(CHECK.validate_links(links())["ifindex"], 1)
        for value in ([], links() * 2, [{"ifname": "eth0"}], [{"ifname": "lo", "link_type": "ether"}],
                      [{"ifname": "lo", "link_type": "loopback", "ifindex": True, "flags": []}]):
            with self.subTest(value=value), self.assertRaises(CHECK.CheckError):
                CHECK.validate_links(value)

    def test_qdisc_initial_and_owned_identity(self) -> None:
        self.assertTrue(CHECK.noqueue_only(noqueue()))
        self.assertFalse(CHECK.noqueue_only([]))
        self.assertFalse(CHECK.noqueue_only(noqueue() + [root_qdisc()]))
        self.assertTrue(CHECK.owned_tree([root_qdisc()], False))
        self.assertTrue(CHECK.owned_tree([root_qdisc(), netem()], True))
        self.assertFalse(CHECK.owned_tree([root_qdisc(), netem()], False))
        for changed in ({"handle": "2:"}, {"kind": "fq"}, {"options": {"bands": 4}}):
            root = root_qdisc() | changed
            self.assertFalse(CHECK.owned_tree([root, netem()], True))
        self.assertFalse(CHECK.owned_tree([root_qdisc(), netem(), {"kind": "fq", "handle": "40:", "parent": "1:2"}], True))
        self.assertFalse(CHECK.owned_tree([root_qdisc(), netem(), netem()], True))

    def test_kernel03_prio_and_netem_options_fixture(self) -> None:
        # Options captured from kernel03; counters below are fixture values.
        tree = json.loads("""[
          {"kind":"prio","handle":"1:","root":true,"options":{
            "bands":3,"priomap":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],"multiqueue":false}},
          {"kind":"netem","handle":"30:","parent":"1:3","options":{
            "limit":10000,"seed":7475289065557766979,"ecn":false,"gap":0},
            "bytes":0,"packets":0,"drops":0,"overlimits":0,"requeues":0,"backlog":0,"qlen":0}
        ]""")
        self.assertTrue(CHECK.owned_tree(tree, True))
        h = harness()
        h.initial_default_confirmed = True
        h.qdisc_attempted = True
        h.netem_attempted = True
        h.remember_netem(tree)
        self.assertEqual(h.netem_options, [tree[1]["options"]])
        self.assertTrue(h.verified_tree(tree))
        changed = copy.deepcopy(tree)
        changed[1]["options"]["seed"] += 1
        self.assertFalse(h.verified_tree(changed))
        changed[1]["options"] = tree[1]["options"] | {"unknown": False}
        self.assertFalse(h.verified_tree(changed))
        for value in (True, 0, None, "false"):
            changed = copy.deepcopy(tree)
            changed[0]["options"]["multiqueue"] = value
            with self.subTest(multiqueue=value):
                self.assertFalse(CHECK.owned_tree(changed, True))
        changed = copy.deepcopy(tree)
        changed[0]["options"]["unknown"] = False
        self.assertFalse(CHECK.owned_tree(changed, True))
        with patch.object(h, "guard", return_value=links(True)[0]), \
             patch.object(h, "qdiscs", side_effect=[tree, noqueue()]), patch.object(h, "tc") as tc:
            self.assertEqual(h.cleanup(), [])
            tc.assert_called_once_with(["qdisc", "del", "dev", "lo", "root", "handle", "1:"])
            self.assertTrue(h.report["qdisc_removed"])
            self.assertFalse(h.qdisc_remaining)

    def test_all_kernel_counters_are_required_and_typed(self) -> None:
        result = CHECK.netem_counters([root_qdisc(), netem()])
        self.assertEqual(set(result), set(CHECK.COUNTERS))
        for key in CHECK.COUNTERS:
            for value in (None, True, -1, "1"):
                with self.subTest(key=key, value=value), self.assertRaises(CHECK.CheckError):
                    CHECK.netem_counters([netem() | {key: value}])
        for value in ([], [netem() | {"parent": "2:3"}], [netem(), netem()]):
            with self.assertRaises(CHECK.CheckError):
                CHECK.netem_counters(value)

    def test_full_receipt_and_pass_required(self) -> None:
        self.assertEqual(CHECK.test_results(test_output(), 0, 1000, 1000), [receipt(), receipt() | {"bytes": 8 << 20}])
        result = receipt() | {"bytes": 8 << 20, "path": "embedded", "private_extra": "not exported"}
        self.assertNotIn("private_extra", CHECK.test_results(test_output(result), 0, 1000, 1000)[0])
        for text, code in ((test_output(), 1), ("PASS\n", 0), (test_output().replace("PASS\n", ""), 0),
                           (test_output() + "--- SKIP: other (0s)\n", 0),
                           (test_output().replace("--- PASS:", "--- SKIP:"), 0),
                           (test_output().replace(CHECK.TAG, "NO_TAG "), 0),
                           (test_output().replace(json.dumps(receipt()), "{}"), 0),
                           (test_output().replace(json.dumps(receipt()), "not-json"), 0)):
            with self.subTest(text=text, code=code), self.assertRaises(CHECK.CheckError):
                CHECK.test_results(text, code, 1000, 1000)

    def test_receipt_rejects_incomplete_or_false_evidence(self) -> None:
        changes = [{"schema": True}, {"uid": 0}, {"gid": 0}, {"bytes": True}, {"bytes": (1 << 20) - 1},
                   {"sha256": "b" * 64}, {"sha256": "x", "expected_sha256": "x"},
                   {"active_after": True}, {"active_after": 1}, {"workers_joined": 1},
                   {"workers_joined": False}, {"path": "unknown"}]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(CHECK.CheckError):
                CHECK.test_results(test_output(receipt() | change), 0, 1000, 1000)
        for key in receipt():
            incomplete = receipt()
            del incomplete[key]
            with self.assertRaises(CHECK.CheckError):
                CHECK.test_results(test_output(incomplete), 0, 1000, 1000)

    def test_receipts_require_both_payload_sizes_once(self) -> None:
        text = test_output()
        single = "\n".join(line for line in text.splitlines() if CHECK.TAG not in line or '"bytes": 1048576' in line)
        for output in (single, text.replace('"bytes": 8388608', '"bytes": 1048576'), text + text):
            with self.assertRaisesRegex(CHECK.CheckError, "missing_or_duplicate_payload_result"):
                CHECK.test_results(output, 0, 1000, 1000)

    def test_actor_and_paths_checked_before_run(self) -> None:
        args = arguments()
        with patch.object(CHECK.pwd, "getpwuid", return_value=SimpleNamespace(pw_gid=1000)), \
             patch.object(CHECK.grp, "getgrgid"), patch.object(Path, "is_file", return_value=True), \
             patch.object(Path, "is_dir", return_value=True), patch.object(Path, "exists", return_value=False), \
             patch.object(Path, "is_symlink", return_value=False), patch.object(Path, "resolve", autospec=True, side_effect=lambda p, **kw: p), \
             patch.object(CHECK.os, "access", return_value=True):
            CHECK.validate_run_args(args)
            for name, value in (("actor_uid", 0), ("actor_gid", 0), ("actor_gid", 1001),
                                ("test_binary", "relative"), ("output", "relative"), ("output", "/tmp/../bad")):
                changed = copy.copy(args)
                setattr(changed, name, value)
                with self.subTest(name=name, value=value), self.assertRaises(CHECK.CheckError):
                    CHECK.validate_run_args(changed)
        with patch.object(CHECK.pwd, "getpwuid", side_effect=KeyError):
            with self.assertRaises(CHECK.CheckError):
                CHECK.validate_run_args(arguments())

    def test_default_dryrun_and_help_have_no_side_effects(self) -> None:
        with patch.object(CHECK.subprocess, "Popen") as popen, patch.object(Path, "mkdir") as mkdir, \
             patch.object(CHECK.os, "open") as opening, patch.object(CHECK, "trusted_tool") as tools, \
             patch.object(CHECK.pwd, "getpwuid") as passwd, contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(CHECK.main([]), 0)
            self.assertTrue(json.loads(out.getvalue())["dry_run"])
            with self.assertRaises(SystemExit) as exited:
                CHECK.main(["--help"])
            self.assertEqual(exited.exception.code, 0)
            for mock in (popen, mkdir, opening, tools, passwd):
                mock.assert_not_called()

    def test_system_tool_resolution_never_uses_path(self) -> None:
        with patch.object(Path, "is_file", return_value=True), patch.object(CHECK.os, "access", return_value=True), \
             patch.object(Path, "resolve", autospec=True, side_effect=lambda p, **kw: p), \
             patch.object(Path, "stat", return_value=SimpleNamespace(st_uid=0, st_mode=0o755)):
            self.assertEqual(CHECK.trusted_tool("tc"), "/usr/sbin/tc")
            with patch.object(Path, "stat", return_value=SimpleNamespace(st_uid=1000, st_mode=0o755)):
                with self.assertRaises(CHECK.CheckError):
                    CHECK.trusted_tool("tc")


class HarnessTests(unittest.TestCase):
    def test_guard_rechecks_netns_root_and_only_lo(self) -> None:
        h = harness()
        def namespace(path: Path) -> SimpleNamespace:
            return SimpleNamespace(st_dev=1, st_ino=20 if str(path) == "/proc/self/ns/net" else 10)
        with patch.object(CHECK.os, "getuid", return_value=0), patch.object(CHECK.os, "geteuid", return_value=0), \
             patch.object(Path, "stat", autospec=True, side_effect=namespace), \
             patch.object(h, "system_json", return_value=links()) as query:
            h.guard()
            self.assertEqual(h.namespace, (1, 20))
            self.assertFalse(h.original_up)
            self.assertEqual(query.call_args.args[0], ["/usr/bin/ip", "-j", "link", "show"])
            with patch.object(h, "system_json", return_value=links() + [{"ifname": "eth0"}]):
                with self.assertRaises(CHECK.CheckError):
                    h.guard()
            h.namespace = (1, 21)
            with self.assertRaises(CHECK.CheckError):
                h.guard()
            h.namespace = (1, 20)
            with patch.object(h, "system_json", return_value=[links()[0] | {"ifindex": 2}]):
                with self.assertRaises(CHECK.CheckError):
                    h.guard()
            with patch.object(Path, "stat", return_value=SimpleNamespace(st_dev=1, st_ino=10)):
                with self.assertRaises(CHECK.CheckError):
                    h.guard()
            with patch.object(CHECK.os, "geteuid", return_value=1000):
                with self.assertRaises(CHECK.CheckError):
                    h.guard()

    def test_every_tc_query_and_write_guards_without_recursion(self) -> None:
        h = harness()
        order = []
        with patch.object(h, "guard", side_effect=lambda: order.append("guard")), \
             patch.object(h, "capture", side_effect=lambda *a, **kw: order.append(a[0]) or b"[]"):
            h.qdiscs()
            h.tc(["qdisc", "del", "dev", "lo", "root", "handle", "1:"])
        self.assertEqual(order[0], "guard")
        self.assertEqual(order[2], "guard")
        self.assertEqual(order[1][0], "/usr/bin/tc")

    def test_setup_ownership_only_after_success_and_fixed_tcp_filter(self) -> None:
        h = harness()
        h.original_up = True
        with patch.object(h, "guard"), patch.object(h, "qdiscs", side_effect=[noqueue(), [root_qdisc(), netem()], [root_qdisc(), netem()]]), \
             patch.object(h, "tc") as tc:
            h.setup()
            self.assertTrue(h.qdisc_owned)
            self.assertTrue(h.netem_owned)
            calls = [call.args[0] for call in tc.call_args_list]
            self.assertEqual(calls[-1], ["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
                                         "match", "ip", "protocol", "6", "0xff", "flowid", "1:3"])
            self.assertFalse(any("replace" in call for call in calls))
        for fail_at in (0, 1):
            h = harness()
            h.original_up = True
            with patch.object(h, "guard"), patch.object(h, "qdiscs", return_value=noqueue()), \
                 patch.object(h, "tc", side_effect=[None] * fail_at + [CHECK.CheckError("injected")]):
                with self.assertRaises(CHECK.CheckError):
                    h.setup()
                self.assertEqual(h.qdisc_owned, fail_at == 1)
                self.assertFalse(h.netem_owned)
        h = harness()
        with patch.object(h, "guard"), patch.object(h, "qdiscs", return_value=[root_qdisc()]), patch.object(h, "tc") as tc, patch.object(h, "set_lo") as lo:
            with self.assertRaises(CHECK.CheckError):
                h.setup()
            tc.assert_not_called()
            lo.assert_not_called()

    def test_down_lo_bootstrap_checks_noqueue_before_add_and_restores_down(self) -> None:
        for initial in ([], noqueue()):
            with self.subTest(initial=initial):
                h = harness()
                h.original_up = False
                events = []
                state = {"up": False}
                snapshots = iter((initial, noqueue(), [root_qdisc(), netem()], [root_qdisc(), netem()],
                                  [root_qdisc(), netem()], noqueue()))

                def query() -> list[dict[str, object]]:
                    result = next(snapshots)
                    events.append(("query", result))
                    return result

                def set_lo(up: bool) -> None:
                    state["up"] = up
                    events.append(("lo", up))

                with patch.object(h, "guard", side_effect=lambda: links(state["up"])[0]), \
                     patch.object(h, "qdiscs", side_effect=query), \
                     patch.object(h, "set_lo", side_effect=set_lo), \
                     patch.object(h, "tc", side_effect=lambda args: events.append(("tc", args))):
                    h.setup()
                    self.assertEqual(events[:3], [("query", initial), ("lo", True), ("query", noqueue())])
                    self.assertEqual(events[3][0], "tc")
                    self.assertEqual(events[3][1][:2], ["qdisc", "add"])
                    self.assertEqual(h.report["initial_qdisc_state"], "empty_down" if initial == [] else "noqueue")
                    self.assertTrue(h.report["lo_brought_up"])
                    self.assertEqual(h.cleanup(), [])
                    self.assertEqual(events[-1], ("lo", False))
                    self.assertTrue(h.report["lo_restored"])
                    self.assertFalse(h.lo_changed)

    def test_bootstrap_rejects_up_empty_and_nondefault_qdiscs(self) -> None:
        noop = [{"kind": "noop", "handle": "0:", "root": True}]
        for up, initial, post_up, changed in ((True, [], None, False),
                                             (False, [root_qdisc()], None, False),
                                             (False, noop, None, False),
                                             (False, [], [], True),
                                             (False, [], [root_qdisc()], True),
                                             (False, [], noop, True),
                                             (False, noqueue(), [], True)):
            with self.subTest(up=up, initial=initial, post_up=post_up):
                h = harness()
                h.original_up = up
                snapshots = [initial, post_up] if changed else [initial]
                state = {"up": up}
                with patch.object(h, "guard", side_effect=lambda: links(state["up"])[0]), \
                     patch.object(h, "qdiscs", side_effect=snapshots), \
                     patch.object(h, "set_lo", side_effect=lambda up: state.update(up=up)) as lo, patch.object(h, "tc") as tc:
                    with self.assertRaisesRegex(CHECK.CheckError, "post_up_qdisc_not_noqueue" if changed else "preexisting_qdisc"):
                        h.setup()
                    tc.assert_not_called()
                    self.assertFalse(h.qdisc_owned)
                    self.assertEqual(h.lo_changed, changed)
                    if changed:
                        lo.assert_called_once_with(True)
                        self.assertEqual(h.cleanup(), [])
                        self.assertEqual([call.args for call in lo.call_args_list], [(True,), (False,)])
                        self.assertTrue(h.report["lo_restored"])
                    else:
                        lo.assert_not_called()

    def test_profile_requires_real_kernel_packets_and_drops(self) -> None:
        for name, count, drops, success in (("baseline", 10, 0, True), ("degraded", 10, 1, True),
                                            ("degraded", 10, 0, False), ("baseline", 0, 0, False)):
            h = harness()
            h.netem_attempted = True
            h.netem_options = [{"limit": 10000}]
            with self.subTest(name=name, drops=drops), patch.object(h, "tc") as tc, \
                 patch.object(h, "qdiscs", side_effect=[[root_qdisc(), netem()], [root_qdisc(), netem(0, 0)], [netem(count, drops)]]), \
                 patch.object(h, "capture", return_value=test_output().encode()) as capture:
                if success:
                    h.profile(name)
                    self.assertTrue(h.report["profiles"][0]["ok"])
                else:
                    with self.assertRaises(CHECK.CheckError):
                        h.profile(name)
                command = capture.call_args.args[0]
                self.assertIn("--clear-groups", command)
                self.assertIn("--no-new-privs", command)
                self.assertIn("--reuid=1000", command)
                self.assertIn("--regid=1000", command)
                self.assertEqual(command[-5:], ["-test.run", "^TestL3ExternalNetem$", "-test.count=1", "-test.v", "-test.timeout=150s"])
                self.assertEqual(tc.call_args.args[0][-4:], ["delay", "200ms" if name == "degraded" else "0ms", "loss", "1%" if name == "degraded" else "0%"])

    def test_failed_test_keeps_partial_kernel_counts(self) -> None:
        h = harness()
        h.netem_attempted = True
        h.netem_options = [{"limit": 10000}]
        with patch.object(h, "tc"), patch.object(h, "qdiscs", side_effect=[[root_qdisc(), netem()], [root_qdisc(), netem(0, 0)], [netem(5, 2)]]), \
             patch.object(h, "capture", side_effect=CHECK.CheckError("test_process_failed")):
            with self.assertRaises(CHECK.CheckError):
                h.profile("degraded")
        self.assertEqual(h.report["profiles"][0]["counter_delta"]["drops"], 2)
        self.assertFalse(h.report["profiles"][0]["ok"])

    def test_cleanup_refuses_foreign_tree_and_joins_children_first(self) -> None:
        for tree in ([root_qdisc() | {"handle": "2:"}], [root_qdisc(), netem(), {"kind": "fq", "handle": "40:"}]):
            h = harness()
            h.qdisc_owned, h.netem_owned = True, True
            h.initial_default_confirmed = True
            child = MagicMock()
            h.children.append(child)
            with patch.object(h, "guard", return_value=links()[0]), patch.object(h, "qdiscs", return_value=tree), patch.object(h, "tc") as tc:
                self.assertEqual(h.cleanup(), ["cleanup_qdisc_options_or_identity_unconfirmed"])
                child.stop.assert_called_once()
                tc.assert_not_called()
        h = harness()
        with patch.object(h, "tc") as tc, patch.object(h, "qdiscs") as query:
            self.assertEqual(h.cleanup(), [])
            query.assert_not_called()
            tc.assert_not_called()

    def test_cleanup_owned_tree_restores_original_lo_and_checks_result(self) -> None:
        h = harness()
        h.qdisc_owned, h.netem_owned, h.lo_changed = True, True, True
        h.original_up = False
        h.initial_default_confirmed = True
        h.netem_attempted = True
        h.netem_options = [{"limit": 10000}]
        with patch.object(h, "qdiscs", side_effect=[[root_qdisc(), netem()], noqueue()]), \
             patch.object(h, "tc") as tc, patch.object(h, "set_lo") as lo, \
             patch.object(h, "guard", side_effect=[links(True)[0], links(True)[0], links()[0]]):
            self.assertEqual(h.cleanup(), [])
            tc.assert_called_once_with(["qdisc", "del", "dev", "lo", "root", "handle", "1:"])
            lo.assert_called_once_with(False)
            self.assertTrue(h.report["qdisc_removed"])
            self.assertTrue(h.report["lo_restored"])
        h = harness()
        h.qdisc_owned = True
        h.initial_default_confirmed = True
        with patch.object(h, "guard", return_value=links(True)[0]), patch.object(h, "qdiscs", side_effect=[[root_qdisc()], [root_qdisc()]]), patch.object(h, "tc"):
            self.assertIn("cleanup_qdisc_remains", h.cleanup())

    def test_applied_setup_mutations_are_reconciled_after_timeout(self) -> None:
        for failure in ("lo-up", "root-add", "netem-add", "filter-add"):
            with self.subTest(failure=failure):
                h = harness()
                with simulated_kernel(h, failure) as state:
                    with self.assertRaisesRegex(CHECK.CheckError, "process_timeout"):
                        h.setup()
                    self.assertTrue(state["injected"])
                    errors = h.cleanup()
                    self.assertFalse(state["up"])
                    self.assertFalse(h.lo_remaining)
                    self.assertTrue(h.report["lo_restored"])
                    self.assertTrue(h.lo_attempted)
                    if failure == "netem-add":
                        # The interrupted add never confirmed a complete options snapshot.
                        self.assertEqual(errors, ["cleanup_qdisc_options_or_identity_unconfirmed"])
                        self.assertIsNone(h.qdisc_remaining)
                        self.assertNotIn("root-delete", state["mutations"])
                        self.assertFalse(h.report.get("qdisc_removed", False))
                    else:
                        self.assertEqual(errors, [])
                        self.assertFalse(h.qdisc_remaining)
                        if failure != "lo-up":
                            self.assertTrue(h.report["qdisc_removed"])
                    if failure != "lo-up":
                        self.assertTrue(h.qdisc_attempted)
                    if failure in ("netem-add", "filter-add"):
                        self.assertTrue(h.netem_attempted)

    def test_unapplied_mutations_do_not_delete_absent_resources(self) -> None:
        for failure in ("lo-up", "root-add", "netem-add"):
            with self.subTest(failure=failure):
                h = harness()
                with simulated_kernel(h, failure, apply=False) as state:
                    with self.assertRaises(CHECK.CheckError):
                        h.setup()
                    self.assertEqual(h.cleanup(), [])
                    self.assertFalse(h.qdisc_remaining)
                    self.assertFalse(h.lo_remaining)
                    self.assertFalse(state["up"])
                    if failure in ("lo-up", "root-add"):
                        self.assertNotIn("root-delete", state["mutations"])
                        self.assertFalse(h.report.get("qdisc_removed", False))
                    else:
                        self.assertTrue(h.report["qdisc_removed"])

    def test_changed_or_unknown_options_are_not_deleted(self) -> None:
        for target in ("root-extra-option", "root-wrong-option", "netem-extra-option", "netem-wrong-handle"):
            with self.subTest(target=target):
                h = harness()
                with simulated_kernel(h) as state:
                    h.setup()
                    if target == "root-extra-option":
                        state["qdiscs"][0]["options"]["unknown"] = True
                    elif target == "root-wrong-option":
                        state["qdiscs"][0]["options"]["bands"] = 4
                    elif target == "netem-extra-option":
                        state["qdiscs"][1]["options"]["unknown"] = True
                    else:
                        state["qdiscs"][1]["handle"] = "40:"
                    self.assertEqual(h.cleanup(), ["cleanup_qdisc_options_or_identity_unconfirmed"])
                    self.assertIsNone(h.qdisc_remaining)
                    self.assertFalse(h.lo_remaining)
                    self.assertNotIn("root-delete", state["mutations"])

    def test_timed_out_netem_change_refuses_unconfirmed_options(self) -> None:
        for apply in (True, False):
            with self.subTest(apply=apply):
                h = harness()
                with simulated_kernel(h, "netem-change", apply=apply) as state:
                    h.setup()
                    with self.assertRaises(CHECK.CheckError):
                        h.profile("degraded")
                    self.assertTrue(h.netem_change_attempted)
                    errors = h.cleanup()
                    self.assertFalse(h.lo_remaining)
                    if apply:
                        self.assertEqual(errors, ["cleanup_qdisc_options_or_identity_unconfirmed"])
                        self.assertIsNone(h.qdisc_remaining)
                        self.assertNotIn("root-delete", state["mutations"])
                    else:
                        self.assertEqual(errors, [])
                        self.assertFalse(h.qdisc_remaining)
                        self.assertIn("root-delete", state["mutations"])

    def test_cleanup_mutations_applied_before_timeout_are_verified(self) -> None:
        for failure in ("root-delete", "lo-down"):
            with self.subTest(failure=failure):
                h = harness()
                with simulated_kernel(h, failure) as state:
                    h.setup()
                    self.assertEqual(h.cleanup(), ["process_timeout"])
                    self.assertTrue(state["injected"])
                    self.assertFalse(h.qdisc_remaining)
                    self.assertFalse(h.lo_remaining)
                    self.assertTrue(h.report["qdisc_removed"])
                    self.assertTrue(h.report["lo_restored"])
                    self.assertFalse(state["up"])

    def test_failed_reconciliation_reports_unknown_remaining_state(self) -> None:
        h = harness()
        with simulated_kernel(h) as state:
            h.setup()
            with patch.object(h, "guard", side_effect=CHECK.CheckError("network_namespace_identity_mismatch")):
                self.assertEqual(h.cleanup(), ["network_namespace_identity_mismatch", "network_namespace_identity_mismatch"])
            self.assertIsNone(h.qdisc_remaining)
            self.assertIsNone(h.lo_remaining)
            self.assertNotIn("root-delete", state["mutations"])
            self.assertNotIn("lo-down", state["mutations"])

    def test_reaped_child_and_repeated_cleanup_never_signal(self) -> None:
        process = MagicMock(pid=4321, returncode=0)
        process.poll.return_value = 0
        child = CHECK.Child(process, {})
        h = harness()
        h.children.append(child)
        with patch.object(CHECK.os, "killpg") as kill:
            child.stop()
            self.assertEqual(h.cleanup(), [])
            self.assertEqual(h.cleanup(), [])
            kill.assert_not_called()
        self.assertTrue(child.stopped)
        self.assertTrue(child.audit["joined"])
        self.assertEqual(child.audit["returncode"], 0)
        process.wait.assert_not_called()

    def test_child_signals_only_owned_group_and_reaps(self) -> None:
        process = MagicMock(pid=4321, returncode=-15)
        process.poll.return_value = None
        child = CHECK.Child(process, {})
        with patch.object(CHECK.os, "killpg") as kill:
            child.stop()
            child.stop()
            kill.assert_called_once_with(4321, signal.SIGTERM)
            self.assertTrue(child.audit["joined"])
            self.assertEqual(child.audit["returncode"], -15)
        process = MagicMock(pid=4321, returncode=-9)
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired("test", 2), None, None]
        with patch.object(CHECK.os, "killpg") as kill:
            CHECK.Child(process, {}).stop()
            self.assertEqual([call.args for call in kill.call_args_list],
                             [(4321, signal.SIGTERM), (4321, signal.SIGKILL)])

    def test_exit_before_escalation_does_not_signal_reaped_group(self) -> None:
        process = MagicMock(pid=4321, returncode=0)
        process.poll.side_effect = [None, None, 0]
        process.wait.side_effect = [subprocess.TimeoutExpired("test", 2), 0]
        child = CHECK.Child(process, {})
        with patch.object(CHECK.os, "killpg") as kill:
            child.stop()
            kill.assert_called_once_with(4321, signal.SIGTERM)
        self.assertTrue(child.audit["joined"])
        self.assertEqual(child.audit["returncode"], 0)

    def test_capture_uses_bounded_private_output_and_minimal_env(self) -> None:
        for test, too_large, interrupted in ((False, False, False), (True, False, False),
                                             (True, True, False), (True, False, True)):
            h = harness()
            h.interrupted = interrupted
            process = MagicMock(pid=42, returncode=0)
            process.stdout.fileno.return_value = 7
            selector = MagicMock()
            selector.get_map.side_effect = [True, False]
            selector.select.return_value = [(SimpleNamespace(fd=7, fileobj=process.stdout), 1)]
            with self.subTest(test=test, too_large=too_large, interrupted=interrupted), \
                 patch.object(h, "private_file", return_value=contextlib.nullcontext(io.BytesIO())), \
                 patch.object(CHECK.subprocess, "Popen", return_value=process) as popen, \
                 patch.object(CHECK.selectors, "DefaultSelector", return_value=contextlib.nullcontext(selector)), \
                 patch.object(CHECK.os, "read", return_value=b"abc"), patch.object(CHECK.Child, "stop") as stop:
                if too_large or interrupted:
                    with self.assertRaises(CHECK.CheckError):
                        h.capture(["/usr/bin/test"], 1, 2 if too_large else 10, "test", test=test)
                else:
                    self.assertEqual(h.capture(["/usr/bin/test"], 1, 10, "test", test=test), b"abc")
                if interrupted:
                    popen.assert_not_called()
                    continue
                stop.assert_called_once()
                kwargs = popen.call_args.kwargs
                self.assertTrue(kwargs["start_new_session"])
                self.assertEqual(set(kwargs["env"]), {"PATH", "LC_ALL", *(["RENET_MANUAL_L3_NETEM"] if test else [])})
                self.assertNotIn("shell", kwargs)
                process.stdout.close.assert_called_once()

    def test_capture_total_budget_expires_without_process(self) -> None:
        h = harness()
        h.deadline = 0
        with patch.object(CHECK.subprocess, "Popen") as popen:
            with self.assertRaises(CHECK.CheckError):
                h.capture(["/usr/bin/test"], 1, 10, "test")
            popen.assert_not_called()

    def test_capture_timeout_and_failed_reap_still_close_pipe(self) -> None:
        h = harness()
        process = MagicMock(pid=42, returncode=-15)
        selector = MagicMock()
        selector.get_map.return_value = True
        with patch.object(h, "private_file", return_value=contextlib.nullcontext(io.BytesIO())), \
             patch.object(CHECK.subprocess, "Popen", return_value=process), \
             patch.object(CHECK.selectors, "DefaultSelector", return_value=contextlib.nullcontext(selector)), \
             patch.object(CHECK.time, "monotonic", side_effect=[0, 0, 100]), \
             patch.object(CHECK.Child, "stop") as stop:
            with self.assertRaisesRegex(CHECK.CheckError, "process_timeout"):
                h.capture(["/usr/bin/test"], 1, 10, "test")
            stop.assert_called_once()
            process.stdout.close.assert_called_once()
            self.assertEqual(h.report["commands"][0]["error"], "process_timeout")
        process.stdout.close.reset_mock()
        selector.get_map.return_value = False
        with patch.object(h, "private_file", return_value=contextlib.nullcontext(io.BytesIO())), \
             patch.object(CHECK.subprocess, "Popen", return_value=process), \
             patch.object(CHECK.selectors, "DefaultSelector", return_value=contextlib.nullcontext(selector)), \
             patch.object(CHECK.Child, "stop", side_effect=RuntimeError("reap failed")):
            with self.assertRaises(RuntimeError):
                h.capture(["/usr/bin/test"], 1, 10, "test")
            process.stdout.close.assert_called_once()

    def test_signaled_run_reaps_before_network_cleanup_and_fails(self) -> None:
        h = harness()
        order = []
        child = MagicMock()
        child.stop.side_effect = lambda: order.append("child")
        h.children.append(child)
        h.qdisc_owned = True
        h.initial_default_confirmed = True
        h.interrupted_handler(signal.SIGTERM, None)
        self.assertTrue(h.interrupted)
        self.assertEqual(h.report["signal"], signal.SIGTERM)
        with patch.object(h, "guard", return_value=links(True)[0]), \
             patch.object(h, "qdiscs", side_effect=lambda: order.append("qdisc") or [root_qdisc()]), \
             patch.object(h, "tc", side_effect=CHECK.CheckError("system_command_failed")):
            self.assertEqual(h.cleanup(), ["cleanup_qdisc_remains"])
        self.assertEqual(order[:2], ["child", "qdisc"])
        self.assertTrue(h.qdisc_owned)

    def test_private_files_are_exclusive_and_not_symlinks(self) -> None:
        h = harness()
        with tempfile.TemporaryDirectory() as directory:
            h.args.output = directory
            with h.private_file("private.log") as output:
                output.write(b"private")
            path = Path(directory) / "private.log"
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            with self.assertRaises(FileExistsError):
                h.private_file("private.log")
            (Path(directory) / "link.log").symlink_to(path)
            with self.assertRaises(FileExistsError):
                h.private_file("link.log")

    def test_main_cleanup_failure_forces_failure_and_redacts_raw_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "result"
            argv = ["--run", "--test-binary", "/tmp/integration.test", "--output", str(output),
                    "--actor-uid", "1000", "--actor-gid", "1000", "--profile", "baseline"]
            with patch.object(CHECK, "validate_run_args"), patch.object(CHECK, "trusted_tool", side_effect=lambda name: "/usr/bin/" + name), \
                 patch.object(CHECK.Harness, "setup"), patch.object(CHECK.Harness, "profile"), \
                 patch.object(CHECK.Harness, "cleanup", return_value=["qdisc_cleanup_failed"]), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(CHECK.main(argv), 1)
            report = json.loads((output / "report.json").read_text())
            self.assertFalse(report["ok"])
            self.assertEqual(report["cleanup_errors"], ["qdisc_cleanup_failed"])
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
            self.assertEqual(stat.S_IMODE((output / "report.json").stat().st_mode), 0o600)
            output = Path(directory) / "failure"
            argv[argv.index("--output") + 1] = str(output)
            with patch.object(CHECK, "validate_run_args"), patch.object(CHECK, "trusted_tool", return_value="/usr/bin/tool"), \
                 patch.object(CHECK.Harness, "setup", side_effect=RuntimeError("secret raw stdout")), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(CHECK.main(argv), 1)
            self.assertNotIn("secret", (output / "report.json").read_text())


if __name__ == "__main__":
    unittest.main()
