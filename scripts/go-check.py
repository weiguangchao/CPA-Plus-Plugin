#!/usr/bin/env python3
"""Run Go tests or vet with test/ files mapped into the source package."""

import argparse
import json
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("test", "vet"))
    args, flags = parser.parse_known_args()
    root = Path(__file__).resolve().parents[1]
    tests = sorted((root / "test").glob("*_test.go"))
    if not tests:
        parser.error("no Go tests found in test/")
    replacements = {}
    for source in tests:
        replacements[str(root / "src" / source.name)] = str(source)
        replacements[str(source)] = ""
    with tempfile.TemporaryDirectory(prefix="cpa-plus-go-check-") as directory:
        overlay = Path(directory) / "overlay.json"
        overlay.write_text(json.dumps({"Replace": replacements}))
        result = subprocess.run(["go", args.command, f"-overlay={overlay}", *flags], cwd=root)
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
