"""Refresh only the marked public-key table in the conformance README."""

from pathlib import Path

from common import DIRECTORY, public_key


def generate(check: bool) -> list[Path]:
    path = DIRECTORY / "README.md"
    if check and not path.is_file():
        return [path]
    original = path.read_bytes()
    start = b"<!-- public-keys:start -->"
    end = b"<!-- public-keys:end -->"
    if check and (original.count(start) != 1 or original.count(end) != 1
                  or original.index(start) > original.index(end)):
        return [path]
    before, table = original.split(start)
    _, after = table.split(end)
    rows = ["| 名称 | 原始公钥（hex） |", "|---|---|"]
    for name in ("center-signing-1", "device-operator", "device-client", "hub-node-1"):
        rows.append(f"| `{name}` | `{public_key(name).hex()}` |")
    updated = before + start + ("\n\n" + "\n".join(rows) + "\n\n").encode() + end + after
    if check:
        return [] if original == updated else [path]
    path.write_bytes(updated)
    return []
