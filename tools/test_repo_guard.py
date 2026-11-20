import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("repo_guard", Path(__file__).with_name("repo-guard.py"))
assert SPEC is not None and SPEC.loader is not None
GUARD = importlib.util.module_from_spec(SPEC)
sys.modules["repo_guard"] = GUARD
SPEC.loader.exec_module(GUARD)

RULES = GUARD.BUILTIN_RULES


def hits(line: str, path: str = "a.txt", allow: list | None = None) -> list[str]:
    return [f.rule for f in GUARD.scan_lines(path, [(1, line)], RULES, allow or [])]


class RuleTests(unittest.TestCase):
    def test_private_ipv4(self) -> None:
        for ip in ("10.1.2.3", "172.16.0.1", "172.31.255.254", "192.168.0.7", "100.64.0.1", "100.127.255.1"):
            self.assertEqual(hits(f"host {ip}:80"), ["private-ipv4"], ip)
        for ip in ("127.0.0.1", "192.0.2.5", "198.51.100.9", "203.0.113.77", "172.32.0.1", "172.15.0.1",
                   "100.128.0.1", "8.8.8.8", "169.254.1.1", "10.0.0.256"):
            self.assertEqual(hits(f"host {ip}"), [], ip)
        self.assertEqual(hits("version 1.10.1.2.3"), [])
        self.assertEqual(hits("10.1.2.3.4"), [])

    def test_vin(self) -> None:
        real_like = "WBA8E9C51KA123456"  # structural VIN
        self.assertEqual(hits(f"vin: {real_like}"), ["vin"])
        valid_check = "1M8GDM9AXKP042788"  # classic ISO 3779 check-digit example
        self.assertTrue(GUARD.vin_check_digit_ok(valid_check))
        self.assertEqual(hits(valid_check), ["vin"])

    def test_vin_false_positives(self) -> None:
        for text in (
            "0123456789ABCDEF0",  # hex
            "a3f5c9e1b2d4f6a8c",  # hex hash prefix
            "ABCDEFGHJKLMNPRST",  # letters only
            "12345678901234567",  # digits only
            "WBA00000000000000",  # few distinct chars
            "WBA8E9C51KA1234567",  # 18 chars
            "xWBA8E9C51KA123456",  # embedded in longer token
            "WBAIOQ8E9C51KA123",  # contains I/O/Q
        ):
            self.assertEqual(hits(text), [], text)

    def test_invite_code(self) -> None:
        body = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
        self.assertEqual(hits(f'code := "renet1:{body}"'), ["invite-code"])
        self.assertEqual(hits('x := "renet1:" + strings.Repeat("A", 1400)'), [])
        self.assertEqual(hits('peer: "renet1:secret"'), [])
        self.assertEqual(hits('"renet1:' + "A" * 60 + '"'), [])

    def test_private_key(self) -> None:
        self.assertEqual(hits("-----BEGIN PRIVATE KEY-----"), ["private-key"])
        self.assertEqual(hits("-----BEGIN OPENSSH PRIVATE KEY-----"), ["private-key"])
        self.assertEqual(hits("-----BEGIN PUBLIC KEY-----"), [])


class AllowTests(unittest.TestCase):
    def test_inline_marker(self) -> None:
        self.assertEqual(hits("10.1.2.3 // repo-guard: allow"), [])

    def test_allowlist_path_rule_text(self) -> None:
        allow = GUARD.parse_allowlist("# c\n*_test.go | private-ipv4\ndocs/* | vin | WBA\n")
        self.assertEqual(hits("10.1.2.3", "x/a_test.go", allow), [])
        self.assertEqual(hits("10.1.2.3", "x/a.go", allow), ["private-ipv4"])
        self.assertEqual(hits("WBA8E9C51KA123456", "docs/a.md", allow), [])
        self.assertEqual(hits("ZZZ8E9C51KA123456", "docs/a.md", allow), ["vin"])

    def test_allowlist_hash(self) -> None:
        digest = GUARD.match_digest("10.1.2.3")[:10]
        allow = GUARD.parse_allowlist(f"* | private-ipv4 | sha256:{digest}\n")
        self.assertEqual(hits("10.1.2.3", allow=allow), [])
        self.assertEqual(hits("10.1.2.4", allow=allow), ["private-ipv4"])

    def test_mask_hides_middle(self) -> None:
        m = GUARD.mask("192.168.100.200")
        self.assertTrue(m.startswith("19") and m.endswith("00"))
        self.assertNotIn("168", m)


class ExtraRuleTests(unittest.TestCase):
    def test_file_and_inline(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "rules.txt"
            p.write_text("# c\n\\bcorp-internal\\b\n")
            env = {"REPO_GUARD_RULES_INLINE": "secret-host-\\d+\n"}
            with mock.patch.dict(os.environ, env):
                rules = GUARD.load_extra_rules(str(p))
            self.assertEqual([r.name for r in rules], ["private-1", "private-2"])
            self.assertEqual(list(rules[0].find("see corp-internal wiki")), ["corp-internal"])
            self.assertEqual(list(rules[1].find("secret-host-42")), ["secret-host-42"])

    def test_env_file_and_missing(self) -> None:
        with mock.patch.dict(os.environ, {"REPO_GUARD_RULES": "/nonexistent/x"}, clear=False):
            with self.assertRaises(SystemExit):
                GUARD.load_extra_rules(None)

    def test_invalid_regex(self) -> None:
        with self.assertRaises(SystemExit):
            GUARD.parse_rules("(", "t")


def run_git(root: Path, *args: str) -> None:
    subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args], cwd=root, check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


class GitModeTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        run_git(self.root, "init", "-q")
        (self.root / "ok.txt").write_text("clean\n")
        run_git(self.root, "add", "ok.txt")
        run_git(self.root, "commit", "-qm", "base")
        self.base = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.root, text=True).strip()

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def run_main(self, *args: str) -> tuple[int, str]:
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(io.StringIO()):
            code = GUARD.main(["--root", str(self.root), *args])
        return code, out.getvalue()

    def test_tracked_skips_binary_and_honours_allowlist(self) -> None:
        (self.root / "bin.dat").write_bytes(b"\0 10.1.2.3")
        (self.root / "t.txt").write_text("a\nip 10.1.2.3\n")
        run_git(self.root, "add", "bin.dat", "t.txt")
        code, out = self.run_main()
        self.assertEqual(code, 1)
        self.assertEqual(out.strip(), "t.txt:2: private-ipv4: 10*****.3".replace("10*****.3", GUARD.mask("10.1.2.3")))
        self.assertNotIn("10.1.2.3", out)
        (self.root / GUARD.ALLOW_FILE).write_text("t.txt | private-ipv4\n")
        self.assertEqual(self.run_main()[0], 0)

    def test_staged_reads_index_not_worktree(self) -> None:
        (self.root / "s.txt").write_text("192.168.1.1\n")
        run_git(self.root, "add", "s.txt")
        (self.root / "s.txt").write_text("clean\n")
        code, out = self.run_main("--staged")
        self.assertEqual(code, 1)
        self.assertTrue(out.startswith("s.txt:1: private-ipv4"))

    def test_rev_range_reports_added_lines_even_if_later_removed(self) -> None:
        (self.root / "r.txt").write_text("x\n100.64.0.9\n")
        run_git(self.root, "add", "r.txt")
        run_git(self.root, "commit", "-qm", "add")
        (self.root / "r.txt").write_text("x\n")
        run_git(self.root, "commit", "-aqm", "remove")
        code, out = self.run_main("--rev-range", f"{self.base}..HEAD")
        self.assertEqual(code, 1)
        self.assertTrue(out.startswith("r.txt:2: private-ipv4"))
        self.assertEqual(self.run_main("--rev-range", "HEAD..HEAD")[0], 0)


if __name__ == "__main__":
    unittest.main()
