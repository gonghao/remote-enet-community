#!/usr/bin/env python3
"""Scan content that is about to become public for private markers.

Built-in rules are generic and safe to publish. Organisation-specific rules
(private domains, hostnames, ...) must never live in this repository: inject
them with --rules FILE, REPO_GUARD_RULES (path) or REPO_GUARD_RULES_INLINE
(content, e.g. a CI secret). One regex per line, '#' starts a comment.

Matches are printed masked; the private text itself never reaches the log.
"""
from __future__ import annotations

import argparse
import fnmatch
import hashlib
import ipaddress
import os
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterable, Iterator

ROOT = Path(__file__).resolve().parent.parent
ALLOW_FILE = ".repo-guard-allow"
INLINE_MARKER = "repo-guard: allow"

PRIVATE_NETS = [ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10")]

IPV4 = re.compile(r"(?<![\w.])(\d{1,3}(?:\.\d{1,3}){3})(?![\w]|\.\d)")
# Candidate VIN: 17 chars, no I/O/Q, not embedded in a longer alphanumeric run.
VIN = re.compile(r"(?<![A-Za-z0-9])[A-HJ-NPR-Z0-9]{17}(?![A-Za-z0-9])", re.IGNORECASE)
# Body must be a static, reasonably long base64url-like run.
INVITE = re.compile(r"renet1:([A-Za-z0-9_\-+/=]{40,})", re.IGNORECASE)
PEM = re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")

TRANSLIT = {**{str(d): d for d in range(10)}}
for letters, value in (("AJ", 1), ("BKS", 2), ("CLT", 3), ("DMU", 4), ("ENV", 5), ("FW", 6), ("GPX", 7), ("HY", 8), ("RZ", 9)):
    for ch in letters:
        TRANSLIT[ch] = value
WEIGHTS = (8, 7, 6, 5, 4, 3, 2, 10, 0, 9, 8, 7, 6, 5, 4, 3, 2)


def vin_check_digit_ok(vin: str) -> bool:
    total = sum(TRANSLIT[c] * w for c, w in zip(vin, WEIGHTS))
    expected = "X" if total % 11 == 10 else str(total % 11)
    return vin[8] == expected


def looks_like_vin(candidate: str) -> bool:
    """Heuristic VIN test.

    Trade-off: the ISO 3779 check digit is mandatory only in North America, so
    requiring it would miss most vehicles this project deals with. A candidate
    passes if its check digit is valid, or if it has the structure of a real
    VIN: mixed letters and digits, a valid model-year character at position 10
    (not U/Z/0), and a numeric serial tail (last 4). It is rejected when it is
    pure hex (hashes, fingerprints), has few distinct characters (zero/
    sequential test fixtures like 0000... or 1234567...), or is a long run of
    one repeated character. Residual false positives (documented examples) go
    to the allowlist.
    """
    v = candidate.upper()
    if not (any(c.isdigit() for c in v) and any(c.isalpha() for c in v)):
        return False
    if all(c in "0123456789ABCDEF" for c in v):
        return False
    if len(set(v)) < 8:
        return False
    if vin_check_digit_ok(v):
        return True
    return v[9] not in "UZ0" and v[-4:].isdigit()


def is_private_ip(text: str) -> bool:
    try:
        addr = ipaddress.IPv4Address(text)
    except ValueError:
        return False
    if any(len(p) > 1 and p.startswith("0") for p in text.split(".")):
        return False
    return any(addr in net for net in PRIVATE_NETS)


def looks_like_invite_body(body: str) -> bool:
    return len(set(body)) >= 8


@dataclass(frozen=True)
class Rule:
    name: str
    find: Callable[[str], Iterator[str]]


def regex_finder(pattern: re.Pattern[str], accept: Callable[[str], bool] | None = None, group: int = 0) -> Callable[[str], Iterator[str]]:
    def find(line: str) -> Iterator[str]:
        for m in pattern.finditer(line):
            text = m.group(group)
            if accept is None or accept(m.group(1) if group == 0 and m.groups() else text):
                yield m.group(0)
    return find


BUILTIN_RULES = [
    Rule("private-ipv4", regex_finder(IPV4, is_private_ip)),
    Rule("vin", regex_finder(VIN, looks_like_vin)),
    Rule("invite-code", regex_finder(INVITE, looks_like_invite_body)),
    Rule("private-key", regex_finder(PEM)),
]


def parse_rules(text: str, source: str) -> list[Rule]:
    rules: list[Rule] = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        try:
            pattern = re.compile(line)
        except re.error as exc:
            raise SystemExit(f"repo-guard: invalid regex in {source} line {len(rules) + 1}: {exc}")
        rules.append(Rule(f"private-{len(rules) + 1}", regex_finder(pattern)))
    return rules


def load_extra_rules(rules_file: str | None) -> list[Rule]:
    rules: list[Rule] = []
    path = rules_file or os.environ.get("REPO_GUARD_RULES")
    if path:
        try:
            content = Path(path).read_text(encoding="utf-8")
        except OSError as exc:
            raise SystemExit(f"repo-guard: cannot read rules file: {exc.strerror}")
        rules += parse_rules(content, "rules file")
    inline = os.environ.get("REPO_GUARD_RULES_INLINE", "")
    if inline.strip():
        offset = len(rules)
        for r in parse_rules(inline, "REPO_GUARD_RULES_INLINE"):
            rules.append(Rule(f"private-{offset + int(r.name.split('-')[1])}", r.find))
    return rules


@dataclass(frozen=True)
class AllowEntry:
    glob: str
    rule: str | None
    text: str | None


def parse_allowlist(content: str) -> list[AllowEntry]:
    """Lines: `path-glob [| rule [| text]]`; rule may be `*`.

    `text` is a substring of the match, or `sha256:<hex prefix>` of the match so
    that the allowlist does not have to contain the private text itself.
    """
    entries: list[AllowEntry] = []
    for raw in content.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = [p.strip() for p in line.split("|", 2)]
        rule = parts[1] if len(parts) > 1 and parts[1] not in ("", "*") else None
        text = parts[2] if len(parts) > 2 and parts[2] else None
        entries.append(AllowEntry(parts[0], rule, text))
    return entries


def match_digest(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def allowed(entries: Iterable[AllowEntry], path: str, rule: str, matched: str) -> bool:
    for e in entries:
        if not (fnmatch.fnmatchcase(path, e.glob) or fnmatch.fnmatchcase(path, e.glob.rstrip("/") + "/*")):
            continue
        if e.rule is not None and e.rule != rule:
            continue
        if e.text is not None:
            if e.text.startswith("sha256:"):
                if not match_digest(matched).startswith(e.text[7:].lower()):
                    continue
            elif e.text not in matched:
                continue
        return True
    return False


def mask(text: str) -> str:
    if len(text) <= 6:
        return "*" * len(text)
    return f"{text[:2]}{'*' * min(len(text) - 4, 8)}{text[-2:]}"


@dataclass(frozen=True)
class Finding:
    path: str
    line: int
    rule: str
    matched: str

    def render(self) -> str:
        return f"{self.path}:{self.line}: {self.rule}: {mask(self.matched)}"


def scan_lines(path: str, lines: Iterable[tuple[int, str]], rules: list[Rule], allow: list[AllowEntry]) -> list[Finding]:
    out: list[Finding] = []
    for number, line in lines:
        if INLINE_MARKER in line:
            continue
        for rule in rules:
            for matched in rule.find(line):
                if not allowed(allow, path, rule.name, matched):
                    out.append(Finding(path, number, rule.name, matched))
    return out


def is_binary(data: bytes) -> bool:
    return b"\0" in data[:8192]


def numbered(data: bytes) -> Iterator[tuple[int, str]]:
    for i, line in enumerate(data.decode("utf-8", errors="replace").splitlines(), 1):
        yield i, line


def git(args: list[str], root: Path = ROOT) -> bytes:
    return subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE).stdout


def load_allowlist(root: Path, data: bytes | None = None) -> list[AllowEntry]:
    if data is None:
        p = root / ALLOW_FILE
        data = p.read_bytes() if p.exists() else b""
    return parse_allowlist(data.decode("utf-8", errors="replace"))


def scan_tracked(root: Path, rules: list[Rule], allow: list[AllowEntry]) -> list[Finding]:
    findings: list[Finding] = []
    for name in git(["ls-files", "-z"], root).decode().split("\0"):
        if not name:
            continue
        p = root / name
        if not p.is_file() or p.is_symlink():
            continue
        data = p.read_bytes()
        if not is_binary(data):
            findings += scan_lines(name, numbered(data), rules, allow)
    return findings


def scan_staged(root: Path, rules: list[Rule], allow: list[AllowEntry]) -> list[Finding]:
    findings: list[Finding] = []
    names = git(["diff", "--cached", "--name-only", "-z", "--diff-filter=ACMR"], root).decode().split("\0")
    for name in filter(None, names):
        data = git(["show", f":{name}"], root)
        if not is_binary(data):
            findings += scan_lines(name, numbered(data), rules, allow)
    return findings


def added_lines(diff: str) -> Iterator[tuple[str, int, str]]:
    path = ""
    line_no = 0
    for raw in diff.splitlines():
        if raw.startswith("+++ "):
            path = raw[6:] if raw.startswith("+++ b/") else ""
        elif raw.startswith("@@"):
            m = re.search(r"\+(\d+)", raw)
            line_no = int(m.group(1)) if m else 0
        elif raw.startswith("+") and path:
            yield path, line_no, raw[1:]
            line_no += 1


def scan_range(root: Path, rev_range: str, rules: list[Rule], allow: list[AllowEntry]) -> list[Finding]:
    # git log -p (not a net diff) so a secret added and removed within the
    # range is still reported: every commit in the range gets published.
    diff = git(["log", "-p", "--no-color", "--no-ext-diff", "--unified=0", "--format=", *rev_range.split()], root).decode("utf-8", errors="replace")
    findings: list[Finding] = []
    seen: set[Finding] = set()
    for path, number, text in added_lines(diff):
        for f in scan_lines(path, [(number, text)], rules, allow):
            if f not in seen:
                seen.add(f)
                findings.append(f)
    return findings


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--staged", action="store_true", help="scan staged content only")
    mode.add_argument("--rev-range", metavar="A..B", help="scan lines added by these commits (git log revision args)")
    ap.add_argument("--show-digest", action="store_true", help="append a sha256 prefix per finding for allowlist text entries (local use only: short secrets such as IPv4 are brute-forceable from it)")
    ap.add_argument("--rules", metavar="FILE", help="extra private regex rules, one per line")
    ap.add_argument("--root", default=str(ROOT), help=argparse.SUPPRESS)
    args = ap.parse_args(argv)

    root = Path(args.root)
    rules = BUILTIN_RULES + load_extra_rules(args.rules)
    allow = load_allowlist(root)
    if args.staged:
        findings = scan_staged(root, rules, allow)
    elif args.rev_range:
        findings = scan_range(root, args.rev_range, rules, allow)
    else:
        findings = scan_tracked(root, rules, allow)
    for f in findings:
        print(f.render() + (f" sha256:{match_digest(f.matched)[:12]}" if args.show_digest else ""))
    if findings:
        print(f"repo-guard: {len(findings)} finding(s); fix, or allowlist via {ALLOW_FILE} / '{INLINE_MARKER}'", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
