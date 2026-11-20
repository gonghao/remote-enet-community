"""Discover sibling generators in filename order."""

import argparse
from pathlib import Path
import runpy
import sys


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    different = []
    for path in sorted(Path(__file__).resolve().parent.glob("*.py")):
        if path.name in {"common.py", "run_all.py"}:
            continue
        module = runpy.run_path(str(path), run_name=path.stem)
        generate = module.get("generate")
        if callable(generate) and generate.__module__ == path.stem:
            different.extend(generate(check=args.check) or [])
    for path in different:
        print(f"stale or missing: {path}", file=sys.stderr)
    return int(bool(different))


if __name__ == "__main__":
    sys.exit(main())
