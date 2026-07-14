#!/usr/bin/env python3
"""Package a BTC09 Mach-O binary as a Finder-launchable macOS app ZIP."""

from __future__ import annotations

import argparse
import plistlib
import re
import stat
import zipfile
from pathlib import Path


APP_NAME = "Bitcoin 09.app"
ZIP_TIMESTAMP = (2026, 1, 1, 0, 0, 0)
VERSION_RE = re.compile(r"v?(\d+)\.(\d+)\.(\d+)\Z")


def archive_info(name: str, mode: int) -> zipfile.ZipInfo:
    info = zipfile.ZipInfo(name, ZIP_TIMESTAMP)
    info.create_system = 3
    info.compress_type = zipfile.ZIP_DEFLATED
    info.external_attr = (stat.S_IFREG | mode) << 16
    return info


def package(binary: Path, arch: str, version: str, output_dir: Path) -> Path:
    match = VERSION_RE.fullmatch(version)
    if match is None:
        raise ValueError("version must look like v0.1.29")
    if arch not in {"apple", "intel"}:
        raise ValueError("arch must be apple or intel")
    if not binary.is_file() or binary.is_symlink() or binary.stat().st_size == 0:
        raise ValueError("binary must be a non-empty regular file")

    plain_version = ".".join(match.groups())
    plist = plistlib.dumps(
        {
            "CFBundleDisplayName": "Bitcoin 09",
            "CFBundleExecutable": "btc09",
            "CFBundleIdentifier": "org.btc09.wallet",
            "CFBundleInfoDictionaryVersion": "6.0",
            "CFBundleName": "Bitcoin 09",
            "CFBundlePackageType": "APPL",
            "CFBundleShortVersionString": plain_version,
            "CFBundleVersion": plain_version,
            "LSApplicationCategoryType": "public.app-category.finance",
            "NSHighResolutionCapable": True,
        },
        fmt=plistlib.FMT_XML,
        sort_keys=True,
    )
    guide = (
        "Bitcoin 09 for macOS\n"
        "\n"
        "1. Move Bitcoin 09.app to Applications if you want to keep it there.\n"
        "2. Open Bitcoin 09.app.\n"
        "3. If macOS blocks the first launch, right-click Bitcoin 09.app, "
        "choose Open, then confirm Open.\n"
        "\n"
        "Only use packages downloaded from the official GitHub release and "
        "verify the ZIP against SHA256SUMS before opening it. This community "
        "build is not Apple-notarized.\n"
    ).encode("utf-8")

    output_dir.mkdir(parents=True, exist_ok=True)
    destination = output_dir / f"btc09-macos-{arch}.zip"
    with zipfile.ZipFile(destination, "w") as archive:
        archive.writestr(
            archive_info(f"{APP_NAME}/Contents/MacOS/btc09", 0o755),
            binary.read_bytes(),
        )
        archive.writestr(
            archive_info(f"{APP_NAME}/Contents/Info.plist", 0o644),
            plist,
        )
        archive.writestr(archive_info("README.txt", 0o644), guide)
    return destination


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--arch", required=True, choices=("apple", "intel"))
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    print(package(args.binary, args.arch, args.version, args.output_dir))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
