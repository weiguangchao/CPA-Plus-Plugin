#!/usr/bin/env python3
"""Package the Linux amd64 plugin, configuration example and installation docs."""

import argparse
import hashlib
from pathlib import Path
import re
import struct
import tarfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("library", type=Path)
    parser.add_argument("--output-dir", type=Path, default=Path("dist"))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    version_file = root / "VERSION"
    version = version_file.read_text().strip()
    if not re.fullmatch(r"0\.1\.\d+", version):
        parser.error("VERSION must use the 0.1.x release series")

    library = args.library.resolve(strict=True)
    with library.open("rb") as source:
        header = source.read(64)
    if (len(header) != 64 or header[:6] != b"\x7fELF\x02\x01"
            or struct.unpack_from("<HH", header, 16) != (3, 62)):
        parser.error("library must be a Linux ELF64 little-endian amd64 shared object")

    args.output_dir.mkdir(parents=True, exist_ok=True)
    archive = args.output_dir / f"cpa-plus_{version}_linux_amd64.tar.gz"
    with tarfile.open(archive, "w:gz") as package:
        package.add(library, arcname="cpa-plus.so")
        package.add(version_file, arcname="VERSION")
        for name in ("config.example.yaml", "README.md", "README_CN.md", "LICENSE"):
            package.add(root / name, arcname=name)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    checksum = archive.with_suffix(archive.suffix + ".sha256")
    checksum.write_text(f"{digest}  {archive.name}\n")
    print(archive)
    print(checksum)


if __name__ == "__main__":
    main()
