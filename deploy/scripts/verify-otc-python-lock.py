#!/usr/bin/env python3
from __future__ import annotations

import importlib.metadata
import re
import sys
from collections.abc import Mapping
from pathlib import Path


BOOTSTRAP_PACKAGES = frozenset({"pip", "setuptools", "wheel"})
_NAME = re.compile(r"^[A-Za-z0-9]+(?:[-_.][A-Za-z0-9]+)*$")


class LockVerificationError(RuntimeError):
    pass


def _canonical_name(value: str) -> str:
    if not _NAME.fullmatch(value):
        raise LockVerificationError("dependency name is not canonicalizable")
    return re.sub(r"[-_.]+", "-", value).lower()


def _locked_versions(text: str) -> dict[str, str]:
    result: dict[str, str] = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.count("==") != 1:
            raise LockVerificationError("lock entries must use one exact == pin")
        name, version = line.split("==", 1)
        canonical = _canonical_name(name)
        if canonical in BOOTSTRAP_PACKAGES:
            raise LockVerificationError("bootstrap packages must not be in the runtime lock")
        if not version or version != version.strip() or any(ch.isspace() for ch in version):
            raise LockVerificationError("dependency version is invalid")
        if canonical in result:
            raise LockVerificationError("dependency lock contains a duplicate name")
        result[canonical] = version
    if not result:
        raise LockVerificationError("dependency lock is empty")
    return result


def verify_lock(text: str, installed: Mapping[str, str]) -> None:
    locked = _locked_versions(text)
    normalized: dict[str, str] = {}
    for name, version in installed.items():
        canonical = _canonical_name(name)
        if canonical in BOOTSTRAP_PACKAGES:
            continue
        if canonical in normalized:
            raise LockVerificationError("installed environment has duplicate names")
        normalized[canonical] = version
    if normalized != locked:
        missing = sorted(locked.keys() - normalized.keys())
        unexpected = sorted(normalized.keys() - locked.keys())
        mismatched = sorted(
            name
            for name in locked.keys() & normalized.keys()
            if locked[name] != normalized[name]
        )
        raise LockVerificationError(
            "runtime lock mismatch "
            f"(missing={','.join(missing) or 'none'}; "
            f"unexpected={','.join(unexpected) or 'none'}; "
            f"version_mismatch={','.join(mismatched) or 'none'})"
        )


def _installed_versions() -> dict[str, str]:
    result: dict[str, str] = {}
    for distribution in importlib.metadata.distributions():
        name = distribution.metadata.get("Name")
        if not name:
            raise LockVerificationError("installed distribution has no name")
        canonical = _canonical_name(name)
        if canonical in result:
            raise LockVerificationError("installed environment has duplicate names")
        result[canonical] = distribution.version
    return result


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        raise LockVerificationError("usage: verify-otc-python-lock.py REQUIREMENTS_LOCK")
    lock_path = Path(argv[1])
    if not lock_path.is_absolute() or not lock_path.is_file():
        raise LockVerificationError("requirements lock must be an absolute file path")
    verify_lock(lock_path.read_text(encoding="ascii"), _installed_versions())
    print("OTC Python dependency lock verified")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main(sys.argv))
    except (LockVerificationError, OSError, UnicodeError) as error:
        print(f"OTC Python dependency verification failed: {error}", file=sys.stderr)
        raise SystemExit(1) from None
