#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import os
import sqlite3
import stat
import sys
from pathlib import Path
from typing import Callable
from urllib.parse import quote


class SnapshotError(RuntimeError):
    pass


def _identity(value: os.stat_result) -> tuple[int, int, int, int]:
    return value.st_dev, value.st_ino, value.st_mode, value.st_nlink


def _absolute_real_directory(path: Path, expected_uid: int) -> tuple[Path, int | None]:
    absolute = Path(os.path.abspath(path))
    entry = os.lstat(absolute)
    if not stat.S_ISDIR(entry.st_mode) or stat.S_ISLNK(entry.st_mode):
        raise SnapshotError("snapshot directory is not a real directory")
    if os.name != "nt" and (
        entry.st_uid != expected_uid or stat.S_IMODE(entry.st_mode) & 0o022
    ):
        raise SnapshotError("snapshot directory ownership or mode is unsafe")
    if os.name != "nt" and absolute.resolve(strict=True) != absolute:
        raise SnapshotError("snapshot directory path contains a symbolic link")
    if os.open not in os.supports_dir_fd:
        return absolute, None
    flags = (
        os.O_RDONLY
        | getattr(os, "O_CLOEXEC", 0)
        | getattr(os, "O_DIRECTORY", 0)
        | getattr(os, "O_NOFOLLOW", 0)
    )
    directory_fd = os.open(absolute, flags)
    opened = os.fstat(directory_fd)
    if not stat.S_ISDIR(opened.st_mode) or _identity(opened)[:2] != _identity(entry)[:2]:
        os.close(directory_fd)
        raise SnapshotError("snapshot directory changed while opening")
    return absolute, directory_fd


def _open_source(
    parent: Path,
    directory_fd: int | None,
    name: str,
    expected_uid: int,
) -> tuple[int, os.stat_result]:
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    if directory_fd is None:
        descriptor = os.open(parent / name, flags)
    else:
        descriptor = os.open(name, flags, dir_fd=directory_fd)
    opened = os.fstat(descriptor)
    if (
        not stat.S_ISREG(opened.st_mode)
        or opened.st_nlink != 1
        or (os.name != "nt" and opened.st_uid != expected_uid)
        or (os.name != "nt" and stat.S_IMODE(opened.st_mode) != 0o600)
    ):
        os.close(descriptor)
        raise SnapshotError("snapshot source ownership, mode, or type is unsafe")
    return descriptor, opened


def _named_stat(parent: Path, directory_fd: int | None, name: str) -> os.stat_result:
    if directory_fd is None:
        return os.lstat(parent / name)
    return os.stat(name, dir_fd=directory_fd, follow_symlinks=False)


def _require_same_name(
    parent: Path,
    directory_fd: int | None,
    name: str,
    opened: os.stat_result,
) -> None:
    try:
        current = _named_stat(parent, directory_fd, name)
    except OSError as exc:
        raise SnapshotError("snapshot source name disappeared") from exc
    if not stat.S_ISREG(current.st_mode) or _identity(current) != _identity(opened):
        raise SnapshotError("snapshot source name changed after validation")


def _relative_path(parent: Path, directory_fd: int | None, name: str) -> str:
    if directory_fd is None:
        return str(parent / name)
    return f"/proc/self/fd/{directory_fd}/{name}"


def _sqlite_uri(path: str, *, read_only: bool) -> str:
    mode = "ro" if read_only else "rwc"
    return f"file:{quote(path, safe='/')}?mode={mode}"


def _target_absent(parent: Path, directory_fd: int | None, name: str) -> None:
    try:
        _named_stat(parent, directory_fd, name)
    except FileNotFoundError:
        return
    raise SnapshotError("snapshot destination entry already exists")


def _unlink_created(parent: Path, directory_fd: int | None, name: str) -> None:
    try:
        if directory_fd is None:
            os.unlink(parent / name)
        else:
            os.unlink(name, dir_fd=directory_fd)
    except FileNotFoundError:
        pass


def snapshot_sources(
    database: str | os.PathLike[str],
    wallet: str | os.PathLike[str],
    destination: str | os.PathLike[str],
    *,
    expected_uid: int,
    before_copy: Callable[[], None] | None = None,
) -> None:
    database_path = Path(database)
    wallet_path = Path(wallet)
    destination_path = Path(destination)
    for value in (database_path, wallet_path, destination_path):
        if not value.is_absolute() or value.name in {"", ".", ".."}:
            raise SnapshotError("snapshot paths must be absolute and canonical")

    opened_directories: list[int] = []
    opened_sources: list[int] = []
    created: list[str] = []
    try:
        db_parent, db_parent_fd = _absolute_real_directory(database_path.parent, expected_uid)
        if db_parent_fd is not None:
            opened_directories.append(db_parent_fd)
        wallet_parent, wallet_parent_fd = _absolute_real_directory(
            wallet_path.parent, expected_uid
        )
        if wallet_parent_fd is not None:
            opened_directories.append(wallet_parent_fd)
        target_parent, target_parent_fd = _absolute_real_directory(
            destination_path, expected_uid
        )
        if target_parent_fd is not None:
            opened_directories.append(target_parent_fd)

        db_fd, db_stat = _open_source(
            db_parent, db_parent_fd, database_path.name, expected_uid
        )
        opened_sources.append(db_fd)
        wallet_fd, wallet_stat = _open_source(
            wallet_parent, wallet_parent_fd, wallet_path.name, expected_uid
        )
        opened_sources.append(wallet_fd)
        _target_absent(target_parent, target_parent_fd, "otc_bot.db")
        _target_absent(target_parent, target_parent_fd, "wallet-mainnet.json")
        if before_copy is not None:
            before_copy()
        _require_same_name(db_parent, db_parent_fd, database_path.name, db_stat)
        _require_same_name(wallet_parent, wallet_parent_fd, wallet_path.name, wallet_stat)

        source_uri = _sqlite_uri(
            _relative_path(db_parent, db_parent_fd, database_path.name),
            read_only=True,
        )
        target_uri = _sqlite_uri(
            _relative_path(target_parent, target_parent_fd, "otc_bot.db"),
            read_only=False,
        )
        source_connection = sqlite3.connect(source_uri, uri=True)
        try:
            if source_connection.execute("PRAGMA integrity_check").fetchone() != ("ok",):
                raise SnapshotError("source database integrity check failed")
            created.append("otc_bot.db")
            target_connection = sqlite3.connect(target_uri, uri=True)
            try:
                source_connection.backup(target_connection)
            finally:
                target_connection.close()
        finally:
            source_connection.close()

        db_output_flags = (
            (os.O_RDWR if os.name == "nt" else os.O_RDONLY)
            | getattr(os, "O_NOFOLLOW", 0)
        )
        if target_parent_fd is None:
            db_output_fd = os.open(target_parent / "otc_bot.db", db_output_flags)
        else:
            db_output_fd = os.open(
                "otc_bot.db", db_output_flags, dir_fd=target_parent_fd
            )
        try:
            db_output = os.fstat(db_output_fd)
            if not stat.S_ISREG(db_output.st_mode) or db_output.st_nlink != 1:
                raise SnapshotError("database backup target is unsafe")
            if os.name != "nt":
                os.fchmod(db_output_fd, 0o600)
            os.fsync(db_output_fd)
        finally:
            os.close(db_output_fd)

        wallet_flags = (
            os.O_WRONLY
            | os.O_CREAT
            | os.O_EXCL
            | getattr(os, "O_NOFOLLOW", 0)
        )
        if target_parent_fd is None:
            wallet_output_fd = os.open(
                target_parent / "wallet-mainnet.json", wallet_flags, 0o600
            )
        else:
            wallet_output_fd = os.open(
                "wallet-mainnet.json",
                wallet_flags,
                0o600,
                dir_fd=target_parent_fd,
            )
        created.append("wallet-mainnet.json")
        source_hash = hashlib.sha256()
        target_hash = hashlib.sha256()
        try:
            while True:
                block = os.read(wallet_fd, 1 << 20)
                if not block:
                    break
                source_hash.update(block)
                view = memoryview(block)
                while view:
                    written = os.write(wallet_output_fd, view)
                    target_hash.update(view[:written])
                    view = view[written:]
            if os.name != "nt":
                os.fchmod(wallet_output_fd, 0o600)
            os.fsync(wallet_output_fd)
        finally:
            os.close(wallet_output_fd)
        if source_hash.digest() != target_hash.digest():
            raise SnapshotError("wallet backup readback differs")
        if target_parent_fd is not None:
            os.fsync(target_parent_fd)
    except SnapshotError:
        for name in reversed(created):
            _unlink_created(destination_path, locals().get("target_parent_fd"), name)
        raise
    except (OSError, sqlite3.Error) as exc:
        for name in reversed(created):
            _unlink_created(destination_path, locals().get("target_parent_fd"), name)
        raise SnapshotError("snapshot operation failed") from exc
    finally:
        for descriptor in opened_sources:
            os.close(descriptor)
        for descriptor in reversed(opened_directories):
            os.close(descriptor)


def main(argv: list[str]) -> int:
    if len(argv) != 4:
        raise SnapshotError("usage: snapshot-otc-backup.py DB WALLET DESTINATION")
    if getattr(os, "geteuid", lambda: 0)() != 0:
        raise SnapshotError("must run as root")
    snapshot_sources(argv[1], argv[2], argv[3], expected_uid=0)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main(sys.argv))
    except SnapshotError as error:
        print(f"OTC backup snapshot failed: {error}", file=sys.stderr)
        raise SystemExit(1) from None
