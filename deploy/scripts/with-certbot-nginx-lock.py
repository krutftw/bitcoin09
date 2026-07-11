#!/usr/bin/env python3
"""Run one command while holding Certbot's Nginx configuration lock."""

from __future__ import annotations

import os
import stat
import subprocess
import sys
from pathlib import Path

try:
    import fcntl
except ImportError:  # pragma: no cover - the production host is Linux
    fcntl = None  # type: ignore[assignment]


CERTBOT_LOCK_PATH = Path("/etc/nginx/.certbot.lock")
MAX_PATH_RACE_RETRIES = 3


class CertbotLockError(RuntimeError):
    pass


class VerifiedCertbotLock:
    def __init__(self, path: Path, owner_uid: int) -> None:
        self.path = path
        self.owner_uid = owner_uid
        self.owner_gid = os.getegid()
        self.fd: int | None = None
        self.identity: tuple[int, int] | None = None

    def _validate(self, details: os.stat_result) -> None:
        if not stat.S_ISREG(details.st_mode):
            raise CertbotLockError("lock path is not a regular file")
        if details.st_nlink != 1:
            raise CertbotLockError("lock path has an unsafe link count")
        if details.st_uid != self.owner_uid or details.st_gid != self.owner_gid:
            raise CertbotLockError("lock path has an unsafe owner")
        if stat.S_IMODE(details.st_mode) != 0o600:
            raise CertbotLockError("lock path has an unsafe mode")

    @staticmethod
    def _identity(details: os.stat_result) -> tuple[int, int]:
        return details.st_dev, details.st_ino

    def acquire(self) -> None:
        if fcntl is None:
            raise CertbotLockError("fcntl locking is unavailable")
        flags = os.O_CREAT | os.O_WRONLY
        flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
        for _attempt in range(MAX_PATH_RACE_RETRIES):
            try:
                fd = os.open(self.path, flags, 0o600)
            except OSError as error:
                raise CertbotLockError("cannot open Certbot lock") from error
            try:
                opened = os.fstat(fd)
                if (
                    stat.S_ISREG(opened.st_mode)
                    and opened.st_nlink == 1
                    and opened.st_uid == self.owner_uid
                    and opened.st_gid == self.owner_gid
                ):
                    os.fchmod(fd, 0o600)
                    opened = os.fstat(fd)
                self._validate(opened)
                try:
                    fcntl.lockf(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except (BlockingIOError, PermissionError) as error:
                    raise CertbotLockError("Certbot Nginx lock is held") from error
                linked = os.lstat(self.path)
                self._validate(linked)
                if self._identity(opened) != self._identity(linked):
                    fcntl.lockf(fd, fcntl.LOCK_UN)
                    os.close(fd)
                    continue
                self.fd = fd
                self.identity = self._identity(opened)
                return
            except CertbotLockError:
                os.close(fd)
                raise
            except OSError as error:
                os.close(fd)
                raise CertbotLockError("cannot verify Certbot lock") from error
        raise CertbotLockError("Certbot lock path changed during acquisition")

    def release(self) -> None:
        fd, identity = self.fd, self.identity
        self.fd = None
        self.identity = None
        if fd is None or identity is None or fcntl is None:
            return
        release_error: BaseException | None = None
        try:
            linked = os.lstat(self.path)
            self._validate(linked)
            if self._identity(linked) != identity:
                raise CertbotLockError("Certbot lock path changed while held")
            os.unlink(self.path)
        except BaseException as error:
            release_error = error
        finally:
            try:
                fcntl.lockf(fd, fcntl.LOCK_UN)
            finally:
                os.close(fd)
        if release_error is not None:
            if isinstance(release_error, CertbotLockError):
                raise release_error
            raise CertbotLockError("cannot safely release Certbot lock") from release_error

    def __enter__(self) -> VerifiedCertbotLock:
        self.acquire()
        return self

    def __exit__(self, *_exception: object) -> None:
        self.release()


def _validate_certbot_parent() -> None:
    parent = CERTBOT_LOCK_PATH.parent
    try:
        linked = os.lstat(parent)
        resolved = parent.resolve(strict=True)
    except OSError as error:
        raise CertbotLockError("Certbot lock parent is unavailable") from error
    if (
        resolved != parent
        or not stat.S_ISDIR(linked.st_mode)
        or linked.st_uid != 0
        or linked.st_gid != 0
        or stat.S_IMODE(linked.st_mode) != 0o755
    ):
        raise CertbotLockError("Certbot lock parent is unsafe")


def main(argv: list[str]) -> int:
    if len(argv) < 2 or not Path(argv[1]).is_absolute():
        print(
            "usage: with-certbot-nginx-lock.py ABSOLUTE_COMMAND [ARG ...]",
            file=sys.stderr,
        )
        return 2
    if os.geteuid() != 0:
        print("Certbot nginx lock failed", file=sys.stderr)
        return 1
    try:
        _validate_certbot_parent()
        environment = os.environ.copy()
        environment["OTC_NGINX_CERTBOT_LOCK_HELD"] = "1"
        with VerifiedCertbotLock(CERTBOT_LOCK_PATH, 0):
            result = subprocess.run(argv[1:], env=environment, check=False)
    except (OSError, CertbotLockError):
        print("Certbot nginx lock failed", file=sys.stderr)
        return 1
    return result.returncode if result.returncode >= 0 else 128 - result.returncode


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
