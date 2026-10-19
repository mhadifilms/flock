#!/usr/bin/env python3
"""Build Flock archives and SHA-256 sums; never publish or modify Git refs."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

from release import dependency_licenses

ROOT = Path(__file__).resolve().parent.parent
TARGETS = ("darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64", "windows_arm64", "windows_amd64")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    value = args.version.removeprefix("v")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value):
        parser.error("version must be MAJOR.MINOR.PATCH")
    source = (ROOT / "internal/flockcli/cli.go").read_text()
    if f'const Release = "{value}"' not in source:
        parser.error("version must match internal/flockcli.Release")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty")
    epoch = int(subprocess.check_output(["git", "show", "-s", "--format=%ct", "HEAD"], cwd=ROOT))
    with tempfile.TemporaryDirectory(prefix="flock-release-") as scratch:
        binaries = {}
        for target in TARGETS:
            system, arch = target.split("_")
            binary = Path(scratch) / target / ("flock.exe" if system == "windows" else "flock")
            binary.parent.mkdir()
            subprocess.run(["go", "build", "-buildvcs=false", "-trimpath", "-tags", "netgo,osusergo,croc_no_tailcat", "-ldflags=-s -w", "-o", str(binary), "."], cwd=ROOT, env=dict(os.environ, CGO_ENABLED="0", GOOS=system, GOARCH=arch, GOTOOLCHAIN="local"), check=True)
            binaries[target] = binary
            print(json.dumps({"event": "built", "target": target}), flush=True)
        notices = dependency_licenses(binaries.values()).replace("Linux release binaries", "Flock release binaries", 1)
        docs = {name: (ROOT / name).read_bytes() for name in ("LICENSE", "UPSTREAM.md", "README.md")}
        docs["THIRD_PARTY_LICENSES.txt"] = notices.encode()
        docs["wordlists-LICENSE.txt"] = (ROOT / "src/codephrase/wordlists/LICENSE.txt").read_bytes()
        for target, binary in binaries.items():
            archive = output / f"flock_{value}_{target}.tar.gz"
            with archive.open("wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=epoch) as compressed, tarfile.open(fileobj=compressed, mode="w") as stream:
                for name, data in {binary.name: binary.read_bytes(), **docs}.items():
                    entry = tarfile.TarInfo(name)
                    entry.size, entry.mtime = len(data), epoch
                    entry.mode = 0o755 if name == binary.name else 0o644
                    stream.addfile(entry, io.BytesIO(data))
        lines = [f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in sorted(output.glob("*.tar.gz"))]
        (output / "SHA256SUMS").write_text("".join(lines))
        print(json.dumps({"event": "packaged", "version": value, "archives": len(lines)}))


if __name__ == "__main__":
    main()
