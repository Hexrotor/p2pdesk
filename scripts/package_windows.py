#!/usr/bin/env python3
"""Build a self-contained P2PDesk Windows installer executable.

The payload is deliberately assembled from the three files that the Sciter
Windows build needs.  The portable packer then embeds that clean directory;
it never packages target/release wholesale.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest().upper()


def display_path(path: Path) -> str:
    """Repo-relative path when possible, so manifests never carry local paths."""
    try:
        return path.relative_to(ROOT).as_posix()
    except ValueError:
        return path.name


def project_version() -> str:
    cargo = (ROOT / "Cargo.toml").read_text(encoding="utf-8")
    match = re.search(r'^version\s*=\s*"([^"]+)"', cargo, re.MULTILINE)
    if match is None:
        raise RuntimeError("version is missing from Cargo.toml")
    return match.group(1)


def parse_packer_metadata(data_file: Path) -> tuple[list[str], str]:
    data = data_file.read_bytes()
    marker = b"rustdesk"
    if not data.startswith(marker):
        raise RuntimeError("portable packer data.bin has an invalid header")
    offset = len(marker)
    files: list[str] = []
    while data[offset : offset + len(marker)] != marker:
        if offset + 4 > len(data):
            raise RuntimeError("portable packer data.bin ended in a file record")
        path_length = int.from_bytes(data[offset : offset + 4], "big")
        offset += 4
        path_end = offset + path_length
        path = data[offset:path_end].decode("utf-8")
        offset = path_end
        if offset + 4 > len(data):
            raise RuntimeError("portable packer data.bin lacks a file length")
        compressed_length = int.from_bytes(data[offset : offset + 4], "big")
        offset += 4 + compressed_length + 32
        if offset > len(data):
            raise RuntimeError("portable packer data.bin has a truncated file")
        files.append(path.replace("\\", "/").lstrip("./"))
    offset += len(marker)
    executable = data[offset:].decode("utf-8")
    return files, executable.replace("\\", "/")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--exe", type=Path, default=ROOT / "target/release/p2pdesk.exe")
    parser.add_argument(
        "--go-dll", type=Path, default=ROOT / "p2pdesk-net/build/p2pdesk_net.dll"
    )
    parser.add_argument("--sciter-dll", type=Path, default=ROOT / "target/release/sciter.dll")
    parser.add_argument("--output-dir", type=Path, default=ROOT / "dist/windows")
    parser.add_argument("--version", default=None)
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args()

    sources = {
        "p2pdesk.exe": args.exe.resolve(),
        "p2pdesk_net.dll": args.go_dll.resolve(),
        "sciter.dll": args.sciter_dll.resolve(),
    }
    for name, path in sources.items():
        if not path.is_file():
            raise FileNotFoundError(f"{name} not found: {path}")

    version = args.version or project_version()
    output_dir = args.output_dir.resolve()
    if output_dir.exists() and any(output_dir.iterdir()):
        if not args.force:
            raise RuntimeError(f"output directory is not empty: {output_dir} (use --force)")
        shutil.rmtree(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="p2pdesk-package-") as temporary:
        stage = Path(temporary) / "payload"
        stage.mkdir()
        for name, source in sources.items():
            shutil.copy2(source, stage / name)

        portable_dir = ROOT / "libs/portable"
        command = [
            sys.executable,
            str(portable_dir / "generate.py"),
            "--folder",
            str(stage),
            "--output",
            str(portable_dir),
            "--executable",
            str(stage / "p2pdesk.exe"),
        ]
        subprocess.run(command, cwd=portable_dir, check=True)

        # `libs/portable` is a member of the root Cargo workspace, so Cargo
        # writes its binary to the workspace target directory.
        packer = ROOT / "target/release/p2pdesk-portable-packer.exe"
        if not packer.is_file():
            raise RuntimeError(f"portable packer was not produced: {packer}")
        files, embedded_executable = parse_packer_metadata(portable_dir / "data.bin")
        expected_files = sorted(sources)
        if sorted(files) != expected_files:
            raise RuntimeError(f"payload mismatch: embedded={files}, expected={expected_files}")
        if embedded_executable.lstrip("./") != "p2pdesk.exe":
            raise RuntimeError(f"portable entrypoint mismatch: {embedded_executable}")

        package_name = f"P2PDesk-{version}-windows-x64-install.exe"
        package = output_dir / package_name
        shutil.copy2(packer, package)

    payload = {
        name: {"source": display_path(path), "bytes": path.stat().st_size, "sha256": sha256(path)}
        for name, path in sources.items()
    }
    manifest = {
        "package": package.name,
        "version": version,
        "platform": "windows-x64",
        "entrypoint": "p2pdesk.exe",
        "payload": payload,
        "embedded_files": sorted(files),
        "embedded_entrypoint": embedded_executable,
        "package_bytes": package.stat().st_size,
        "package_sha256": sha256(package),
        "command": "python scripts/package_windows.py --force",
    }
    (output_dir / "MANIFEST.json").write_text(
        json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
    )
    (output_dir / "README.txt").write_text(
        "P2PDesk Windows package\n"
        "\n"
        "Double-click the package to install: it first extracts a clean\n"
        "three-file runtime directory, then opens the P2PDesk installer UI.\n"
        "Do not start the install flow from target\\release directly.\n"
        "\n"
        f"Package: {package.name}\n"
        f"SHA256: {manifest['package_sha256']}\n",
        encoding="utf-8",
    )
    print(json.dumps(manifest, indent=2))
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (FileNotFoundError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"package_windows: {error}", file=sys.stderr)
        raise SystemExit(1)
