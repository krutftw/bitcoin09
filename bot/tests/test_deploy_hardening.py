from __future__ import annotations

import json
import importlib.util
import os
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import MagicMock, patch


ROOT = Path(__file__).resolve().parents[2]
BOT_UNIT = ROOT / "bot" / "btc09-otc-bot.service"
FEED_UNIT = ROOT / "bot" / "btc09-otc-feed.service"
SCRIPTS = ROOT / "deploy" / "scripts"
BACKUP = SCRIPTS / "backup-otc.sh"
HEALTH = SCRIPTS / "check-otc-health.sh"
RESTART = SCRIPTS / "test-otc-systemd-restart.sh"
ADAPTER = SCRIPTS / "otc-systemd-restart-adapter.py"
PREFLIGHT = SCRIPTS / "preflight-otc-migration.py"
PROCESS_ENV = SCRIPTS / "check-otc-process-environment.py"
ISOLATION = SCRIPTS / "check-otc-service-isolation.sh"
INSTALL_GENERATION = SCRIPTS / "install-otc-generation.sh"
INSTALL_NGINX = SCRIPTS / "install-otc-nginx.sh"
PATCH_NGINX = SCRIPTS / "patch-otc-nginx-site.py"
CERTBOT_NGINX_LOCK = SCRIPTS / "with-certbot-nginx-lock.py"
NGINX_READINESS = SCRIPTS / "wait-otc-nginx-readback.py"
SNAPSHOT_BACKUP = SCRIPTS / "snapshot-otc-backup.py"
VERIFY_LOCK = SCRIPTS / "verify-otc-python-lock.py"
BOT_README = ROOT / "bot" / "README.md"
DEPLOY_README = ROOT / "deploy" / "README.md"
REQUIREMENTS_LOCK = ROOT / "bot" / "requirements.lock"
NGINX_HTTP = ROOT / "deploy" / "nginx" / "bitcoin09-otc-http.conf"
NGINX_SERVER = ROOT / "deploy" / "nginx" / "bitcoin09-otc-server.conf"
SEED_UNIT = ROOT / "deploy" / "systemd" / "btc09-seed.service"
GIT_BASH = Path(r"C:\Program Files\Git\bin\bash.exe")
SYNTAX_BASH = GIT_BASH if os.name == "nt" else Path(shutil.which("bash") or "/usr/bin/bash")
SHELLCHECK = (
    Path(r"C:\Users\Administrator\AppData\Local\Microsoft\WinGet\Links\shellcheck.exe")
    if os.name == "nt"
    else Path(shutil.which("shellcheck") or "/usr/bin/shellcheck")
)


class _HealthHandler(BaseHTTPRequestHandler):
    payload: object = None
    status = 200

    def do_GET(self) -> None:
        encoded = json.dumps(self.payload, separators=(",", ":")).encode("utf-8")
        self.send_response(self.status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, format: str, *args: object) -> None:
        pass


def _healthy(*, accepting: bool = False) -> dict[str, object]:
    return {
        "integrity": "ok",
        "foreign_key_integrity": "ok",
        "explorer_snapshot_reachable": True,
        "explorer_tx_status_reachable": True,
        "wallet_spendable_units": 0,
        "customer_liability_units": 0,
        "pending_platform_outflow_units": 0,
        "provisional_restricted_units": 0,
        "common_ledger_tip": {"hash": "a" * 64, "height": 1},
        "stale_watched_address_count": 0,
        "gross_fee_units": 0,
        "available_fee_units": 0,
        "negative_fee_invariant": False,
        "transfer_counts": {
            "queued": 0,
            "reserved": 0,
            "prepared": 0,
            "broadcast": 0,
            "uncertain": 0,
        },
        "credited_noncanonical_count": 0,
        "unknown_spend_count": 0,
        "deposit_allocation": {
            "lifetime_count": 0,
            "daily_count": 0,
            "pending_count": 0,
            "lifetime_headroom": 5000,
            "daily_headroom": 100,
        },
        "accepting_orders": accepting,
        "checked_at": 1,
        "feed_age_seconds": 0,
    }


def _initialized_db(path: Path) -> None:
    connection = sqlite3.connect(path)
    try:
        connection.executescript(
            """
            PRAGMA foreign_keys=ON;
            CREATE TABLE schema_meta(
              id INTEGER PRIMARY KEY CHECK(id=1),
              version INTEGER NOT NULL,
              origin TEXT NOT NULL
            );
            INSERT INTO schema_meta VALUES(1,4,'fresh');
            CREATE TABLE orders(order_id INTEGER PRIMARY KEY, state TEXT NOT NULL);
            CREATE TABLE transfers(transfer_id INTEGER PRIMARY KEY, state TEXT NOT NULL);
            """
        )
        connection.commit()
    finally:
        connection.close()


class ServiceUnitTests(unittest.TestCase):
    def test_seed_unit_exposes_only_p2p_and_keeps_http_services_local(self) -> None:
        text = SEED_UNIT.read_text(encoding="utf-8")
        required = {
            "ExecStart=/opt/btc09/btc09 node -listen 0.0.0.0:9009 -explorer 127.0.0.1:8009 -wallet-gateway 127.0.0.1:8010 -solo-api 127.0.0.1:9010 -pplns-state /opt/btc09/data/pplns-window.json -pplns-window 256 -pplns-max-addresses 64 -datadir /opt/btc09/data -seeds 103.80.18.140:9009,108.190.240.138:9009",
            "Restart=always",
            "MemoryMax=1G",
            "UMask=0077",
            "SupplementaryGroups=btc09-otc",
            "CapabilityBoundingSet=",
            "NoNewPrivileges=true",
            "PrivateTmp=true",
            "PrivateDevices=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "ProtectKernelTunables=true",
            "ProtectKernelModules=true",
            "ProtectControlGroups=true",
            "ProtectClock=true",
            "ProtectHostname=true",
            "ProtectKernelLogs=true",
            "LockPersonality=true",
            "RestrictSUIDSGID=true",
            "RestrictNamespaces=true",
            "RestrictRealtime=true",
            "ReadWritePaths=/opt/btc09/data",
            "InaccessiblePaths=-/var/lib/btc09-otc -/etc/btc09",
            "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
        }
        self.assertFalse(required.difference(text.splitlines()))
        self.assertNotIn("-explorer 0.0.0.0:8009", text)

    def test_bot_unit_is_dedicated_disabled_and_hardened(self) -> None:
        text = BOT_UNIT.read_text(encoding="utf-8")
        required = {
            "User=btc09-otc",
            "Group=btc09-otc",
            "StateDirectory=btc09-otc",
            "StateDirectoryMode=0700",
            "ConditionPathExists=!/var/lib/btc09-otc-maintenance/active",
            "WorkingDirectory=/opt/btc09/bitcoin09",
            "Environment=OTC_ACCEPTING_ORDERS=1",
            "Environment=DB_PATH=/var/lib/btc09-otc/otc_bot.db",
            "Environment=BTC09_WALLET_PATH=/var/lib/btc09-otc/wallet-mainnet.json",
            "Environment=PUBLIC_FEED_PATH=/var/lib/btc09-otc-public/otc-bot-feed.json",
            "Environment=BTC09_NETWORK=btc09-mainnet",
            "LoadCredential=otc-secrets:/etc/btc09/otc-secrets.env",
            "Environment=OTC_SECRETS_FILE=%d/otc-secrets",
            "ExecStart=/opt/btc09/venv/bin/python -m bot.btc09_otc_bot",
            "KillMode=control-group",
            "TimeoutStopSec=15",
            "UMask=0077",
            "CapabilityBoundingSet=",
            "NoNewPrivileges=true",
            "PrivateTmp=true",
            "PrivateDevices=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "ProtectKernelTunables=true",
            "ProtectKernelModules=true",
            "ProtectControlGroups=true",
            "ProtectClock=true",
            "ProtectHostname=true",
            "ProtectKernelLogs=true",
            "LockPersonality=true",
            "RestrictSUIDSGID=true",
            "RestrictNamespaces=true",
            "RestrictRealtime=true",
            "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
            "ReadWritePaths=/var/lib/btc09-otc /var/lib/btc09-otc-public /opt/btc09/data/blocks-mainnet.dat.lock",
            "ReadOnlyPaths=/opt/btc09/data",
            "InaccessiblePaths=-/opt/btc09/data/wallet-mainnet.json",
            "ProtectProc=invisible",
            "ProcSubset=pid",
        }
        self.assertFalse(required.difference(text.splitlines()))
        self.assertNotIn("EnvironmentFile=", text)
        self.assertNotIn("ExecStartPre=+", text)
        read_write_paths = next(
            line for line in text.splitlines() if line.startswith("ReadWritePaths=")
        )
        self.assertIn("/opt/btc09/data/blocks-mainnet.dat.lock", read_write_paths)
        self.assertNotIn(" /opt/btc09/data/blocks-mainnet.dat ", f" {read_write_paths} ")
        self.assertNotIn("/opt/btc09/data/wallet-mainnet.json", read_write_paths)
        self.assertNotIn(
            "Environment=BTC09_WALLET_PATH=/opt/btc09/data/wallet-mainnet.json",
            text,
        )

    def test_feed_unit_is_local_read_only_and_cannot_reach_private_state(self) -> None:
        text = FEED_UNIT.read_text(encoding="utf-8")
        required = {
            "User=btc09-otc-feed",
            "Group=btc09-otc-feed",
            "ConditionPathExists=!/var/lib/btc09-otc-maintenance/active",
            "WorkingDirectory=/opt/btc09/bitcoin09",
            "RuntimeDirectory=btc09-otc-feed",
            "RuntimeDirectoryMode=0755",
            "Environment=OTC_FEED_PATH=/run/btc09-otc-feed/public/otc-bot-feed.json",
            "Environment=OTC_FEED_LISTEN=127.0.0.1",
            "ExecStart=/opt/btc09/venv/bin/python -m bot.serve_otc_feed",
            "KillMode=control-group",
            "TimeoutStopSec=15",
            "UMask=0077",
            "CapabilityBoundingSet=",
            "NoNewPrivileges=true",
            "PrivateTmp=true",
            "PrivateDevices=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "ProtectKernelTunables=true",
            "ProtectKernelModules=true",
            "ProtectControlGroups=true",
            "ProtectClock=true",
            "ProtectHostname=true",
            "ProtectKernelLogs=true",
            "LockPersonality=true",
            "RestrictSUIDSGID=true",
            "RestrictNamespaces=true",
            "RestrictRealtime=true",
            "RestrictAddressFamilies=AF_UNIX AF_INET",
            "BindReadOnlyPaths=/var/lib/btc09-otc-public:/run/btc09-otc-feed/public",
            "InaccessiblePaths=-/var/lib/btc09-otc -/var/lib/btc09-otc-public",
            "ProtectProc=invisible",
            "ProcSubset=pid",
            "IPAddressDeny=any",
            "IPAddressAllow=localhost",
            "SocketBindDeny=any",
            "SocketBindAllow=tcp:8019",
        }
        self.assertFalse(required.difference(text.splitlines()))
        self.assertNotIn("User=btc09-otc\n", text)
        self.assertNotIn("ReadWritePaths=", text)
        publisher = (ROOT / "bot" / "otc" / "public_feed.py").read_text(
            encoding="utf-8"
        )
        self.assertNotIn("os.chmod(target.parent", publisher)
        self.assertIn("os.lstat", publisher)
        self.assertIn("dir_fd=directory_fd", publisher)
        self.assertIn("os.fchmod(handle.fileno(), 0o644)", publisher)


class ScriptStaticTests(unittest.TestCase):
    def test_scripts_exist_are_executable_and_parse(self) -> None:
        for script in (BACKUP, HEALTH, RESTART):
            with self.subTest(script=script.name):
                self.assertTrue(script.is_file())
                index = subprocess.run(
                    ["git", "ls-files", "--stage", str(script.relative_to(ROOT))],
                    cwd=ROOT,
                    check=True,
                    capture_output=True,
                    text=True,
                ).stdout
                self.assertTrue(index.startswith("100755 "), index)
                subprocess.run(
                    [str(SYNTAX_BASH), "-n", str(script)],
                    cwd=ROOT,
                    check=True,
                    capture_output=True,
                    text=True,
                )
                subprocess.run(
                    [str(SHELLCHECK), "--severity=warning", str(script)],
                    cwd=ROOT,
                    check=True,
                    capture_output=True,
                    text=True,
                )

    def test_backup_script_has_fail_closed_backup_contract(self) -> None:
        text = BACKUP.read_text(encoding="utf-8")
        for fragment in (
            "set -euo pipefail",
            "/var/backups/btc09",
            "id -u",
            "realpath",
            "sqlite3",
            ".backup",
            "PRAGMA integrity_check",
            "sha256sum",
            "manifest.sha256",
            "stat -c %h",
            "stat -c %a",
            "sync",
            "trap",
            "/var/lib/btc09-otc-maintenance/active",
            "ConditionPathExists=!/var/lib/btc09-otc-maintenance/active",
            "systemctl stop",
            "pgrep -u",
            "ControlGroup",
            "cgroup.procs",
            "grep -Eq '^[0-9]+$' \"$cgroup_file\"",
            'chown root:root "$STATE"',
            "snapshot-otc-backup.py",
            'exec 8>"$MAINTENANCE_DIR/.generation.lock"',
            "flock -n 8",
        ):
            self.assertIn(fragment, text)
        self.assertNotIn("/opt/btc09/data/wallet-mainnet.json", text)
        self.assertLess(
            text.index("systemctl stop"), text.index('chown root:root "$STATE"')
        )
        self.assertLess(
            text.index('exec 8>"$MAINTENANCE_DIR/.generation.lock"'),
            text.index('exec 9>"$physical_destination/.backup.lock"'),
        )
        self.assertLess(
            text.index('chown root:root "$STATE"'),
            text.index('python3 "$SNAPSHOT_HELPER"'),
        )
        self.assertTrue(SNAPSHOT_BACKUP.is_file())

    def test_restart_tool_is_explicit_isolated_and_cgroup_aware(self) -> None:
        text = RESTART.read_text(encoding="utf-8")
        for fragment in (
            "set -euo pipefail",
            "id -u",
            "--acknowledge-service-restart",
            "OTC_ACCEPTING_ORDERS=0",
            "systemctl restart btc09-otc-bot.service",
            "ControlGroup",
            "cgroup.procs",
            "prepared",
            "expected_txid",
            "check-otc-process-environment.py",
            "trap",
            "TEST_PARENT=/run/btc09-otc-restart-test",
            "install -d -o root -g btc09-otc -m 0710",
            '"0:$bot_group_id:710"',
            'runuser -u btc09-otc -- test -x "$TEST_PARENT"',
            'runuser -u btc09-otc -- test ! -w "$TEST_PARENT"',
            'mktemp -d -- "$TEST_PARENT/run.XXXXXXXXXX"',
            "ReadWritePaths=$test_root",
        ):
            self.assertIn(fragment, text)
        self.assertNotIn("/var/lib/btc09-otc/restart-test", text)
        self.assertNotIn(
            'install -d -o btc09-otc -g btc09-otc -m 0700 -- "$test_root"', text
        )
        self.assertTrue(ADAPTER.is_file())

    def test_process_environment_verifier_is_root_only_and_sanitized(self) -> None:
        text = PROCESS_ENV.read_text(encoding="utf-8")
        self.assertIn("os.geteuid()", text)
        self.assertIn("/proc", text)
        self.assertNotIn("BOT_TOKEN", text)
        self.assertNotIn("DISCORD_BOT_TOKEN", text)
        index = subprocess.run(
            ["git", "ls-files", "--stage", str(PROCESS_ENV.relative_to(ROOT))],
            cwd=ROOT,
            capture_output=True,
            text=True,
        ).stdout
        self.assertTrue(index.startswith("100755 "), index)

    def test_backup_snapshot_and_lock_verifier_are_executable_python(self) -> None:
        for script in (
            SNAPSHOT_BACKUP,
            VERIFY_LOCK,
            PATCH_NGINX,
            CERTBOT_NGINX_LOCK,
            NGINX_READINESS,
        ):
            with self.subTest(script=script.name):
                self.assertTrue(script.is_file())
                index = subprocess.run(
                    ["git", "ls-files", "--stage", str(script.relative_to(ROOT))],
                    cwd=ROOT,
                    capture_output=True,
                    text=True,
                ).stdout
                self.assertTrue(index.startswith("100755 "), index)
                subprocess.run(
                    [sys.executable, "-m", "py_compile", str(script)],
                    cwd=ROOT,
                    check=True,
                    capture_output=True,
                    text=True,
                )

    def test_isolation_and_generation_tools_are_executable_and_fail_closed(
        self,
    ) -> None:
        for script in (ISOLATION, INSTALL_GENERATION, INSTALL_NGINX):
            with self.subTest(script=script.name):
                self.assertTrue(script.is_file())
                index = subprocess.run(
                    ["git", "ls-files", "--stage", str(script.relative_to(ROOT))],
                    cwd=ROOT,
                    capture_output=True,
                    text=True,
                ).stdout
                self.assertTrue(index.startswith("100755 "), index)
                subprocess.run(
                    [str(SYNTAX_BASH), "-n", str(script)],
                    cwd=ROOT,
                    check=True,
                    capture_output=True,
                    text=True,
                )
        isolation = ISOLATION.read_text(encoding="utf-8")
        for fragment in (
            "btc09-otc-feed",
            "nsenter",
            "runuser",
            "/proc",
            "wallet-mainnet.json",
            "otc_bot.db",
            "/run/credentials/btc09-otc-bot.service/otc-secrets",
            "/run/btc09-otc-feed/public/otc-bot-feed.json",
            "/var/lib/btc09-otc-public",
            "findmnt",
            "otc-secrets",
            "for _attempt in range(100)",
            "CHAIN_LOCK=/opt/btc09/data/blocks-mainnet.dat.lock",
            "CHAIN_BLOCKS=/opt/btc09/data/blocks-mainnet.dat",
            "0:$bot_group_id:660",
            "0:$bot_group_id:640",
            "st_nlink != 1",
            "os.O_RDWR",
            "os.O_WRONLY",
            "chain lock is not writable inside the bot mount",
            "chain block file is writable inside the bot mount",
            "os.listxattr",
            "system.posix_acl_",
            "follow_symlinks=False",
        ):
            self.assertIn(fragment, isolation)
        generation = INSTALL_GENERATION.read_text(encoding="utf-8")
        for fragment in (
            "/var/lib/btc09-otc-maintenance/active",
            "systemctl daemon-reload",
            "systemctl stop",
            "sqlite3",
            "PRAGMA integrity_check",
            "sha256sum",
            "sync",
            "chown btc09-otc:btc09-otc",
            'staging="$MAINTENANCE_DIR/.generation.$$',
        ):
            self.assertIn(fragment, generation)
        self.assertNotRegex(generation, r"trap[^\n]*rm[^\n]*active")
        self.assertNotIn('staging="$STATE/.generation.', generation)
        self.assertGreaterEqual(
            generation.count('"$STATE/otc_bot.db-journal"'),
            2,
            "stale rollback journals must be removed before and after replacement",
        )


class BackupBehaviorTests(unittest.TestCase):
    def setUp(self) -> None:
        if not GIT_BASH.exists():
            self.skipTest("Git Bash is unavailable")
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.fake = self.root / "fake-bin"
        self.fake.mkdir()
        self.db = self.root / "otc.db"
        self.wallet = self.root / "wallet-mainnet.json"
        _initialized_db(self.db)
        self.wallet.write_text('{"network":"btc09-mainnet"}', encoding="ascii")
        helper = self.fake / "sqlite_helper.py"
        helper.write_text(
            textwrap.dedent(
                r"""
                import sqlite3
                import subprocess
                import sys
                source, command = sys.argv[1:]
                source = subprocess.check_output([r"C:\Program Files\Git\usr\bin\cygpath.exe", "-w", source], text=True).strip()
                if command.startswith(".backup '") and command.endswith("'"):
                    destination = command[9:-1]
                    destination = subprocess.check_output([r"C:\Program Files\Git\usr\bin\cygpath.exe", "-w", destination], text=True).strip()
                    source_db = sqlite3.connect(source)
                    target_db = sqlite3.connect(destination)
                    source_db.backup(target_db)
                    target_db.close()
                    source_db.close()
                elif command == "PRAGMA integrity_check;":
                    connection = sqlite3.connect(source)
                    print(connection.execute(command).fetchone()[0])
                    connection.close()
                else:
                    raise SystemExit(2)
                """
            ).lstrip(),
            encoding="utf-8",
        )
        (self.fake / "id").write_text("#!/usr/bin/env bash\necho 0\n", encoding="utf-8")
        (self.fake / "flock").write_text(
            "#!/usr/bin/env bash\nexit 0\n", encoding="utf-8"
        )
        (self.fake / "chmod").write_text(
            "#!/usr/bin/env bash\nexit 0\n", encoding="utf-8"
        )
        (self.fake / "mkdir").write_text(
            '#!/usr/bin/env bash\nargs=()\nwhile (($#)); do case $1 in -m) shift 2;; *) args+=("$1"); shift;; esac; done\n/usr/bin/mkdir "${args[@]}"\n',
            encoding="utf-8",
        )
        (self.fake / "install").write_text(
            '#!/usr/bin/env bash\ndirectory=0; args=()\nwhile (($#)); do case $1 in -d) directory=1; shift;; -o|-g|-m) shift 2;; --) shift;; *) args+=("$1"); shift;; esac; done\nif ((directory)); then mkdir -p -- "${args[@]}"; else cp -- "${args[@]}"; fi\n',
            encoding="utf-8",
        )
        (self.fake / "sqlite3").write_text(
            f"#!/usr/bin/env bash\n/c/Python314/python.exe '{helper.as_posix()}' \"$@\"\n",
            encoding="utf-8",
        )
        (self.fake / "python3").write_text(
            '#!/usr/bin/env bash\nexec /c/Python314/python.exe "$@"\n',
            encoding="utf-8",
        )
        (self.fake / "stat").write_text(
            textwrap.dedent(
                """
                #!/usr/bin/env bash
                if [[ $1 == -c && $2 == %u ]]; then
                  echo 0
                elif [[ $1 == -c && $2 == %u:%a ]]; then
                  if [[ ${FAKE_UNSAFE_PATH:-} == "${@: -1}" ]]; then echo 0:644
                  elif [[ ${@: -1} == */wallet-mainnet.json || ${@: -1} == *.db ]]; then echo 0:600
                  else echo 0:700
                  fi
                elif [[ $1 == -c && $2 == %u:%g:%a ]]; then
                  if [[ ${FAKE_MAINTENANCE_PATH:-} == "${@: -1}" ]]; then echo 0:0:755
                  else echo 0:0:700
                  fi
                elif [[ $1 == -c && $2 == %a ]]; then
                  if [[ ${FAKE_UNSAFE_PATH:-} == "${@: -1}" ]]; then echo 644
                  elif [[ ${@: -1} == */wallet-mainnet.json || ${@: -1} == *.db ]]; then echo 600
                  else echo 700
                  fi
                else
                  /usr/bin/stat "$@"
                fi
                """
            ).lstrip(),
            encoding="utf-8",
        )
        for file in self.fake.iterdir():
            if file.suffix != ".py":
                os.chmod(file, 0o755)
        self.destination_name = f"test-{os.getpid()}-{id(self)}"
        self.destination = f"/var/backups/btc09/{self.destination_name}"

    def tearDown(self) -> None:
        subprocess.run(
            [str(GIT_BASH), "-lc", 'rm -rf -- "$1"', "bash", self.destination],
            capture_output=True,
        )
        self.temp.cleanup()

    def _unix(self, path: Path) -> str:
        return subprocess.run(
            [str(GIT_BASH), "-lc", 'cygpath -u "$1"', "bash", str(path)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()

    def _run(
        self,
        db: Path | None = None,
        wallet: Path | None = None,
        destination: str | None = None,
        *,
        unsafe: Path | str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["PATH"] = f"{self._unix(self.fake)}:/usr/bin:/bin:/c/Python314"
        if unsafe is not None:
            env["FAKE_UNSAFE_PATH"] = (
                unsafe if isinstance(unsafe, str) else self._unix(unsafe)
            )
        return subprocess.run(
            [
                str(GIT_BASH),
                "-c",
                'export PATH="$1:$PATH"; exec "$2" "$3" "$4" "$5"',
                "bash",
                self._unix(self.fake),
                self._unix(BACKUP),
                self._unix(db or self.db),
                self._unix(wallet or self.wallet),
                destination or self.destination,
            ],
            cwd=ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=15,
        )

    def test_backup_is_consistent_manifested_and_non_overwriting(self) -> None:
        result = self._run()
        self.assertEqual(result.returncode, 0, result.stderr)
        published = result.stdout.strip().split(": ", 1)[1]
        published_windows = subprocess.run(
            [str(GIT_BASH), "-lc", 'cygpath -w "$1"', "bash", published],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        backup_dir = Path(published_windows)
        backup_db = backup_dir / "otc_bot.db"
        connection = sqlite3.connect(backup_db)
        try:
            self.assertEqual(
                connection.execute("PRAGMA integrity_check").fetchone()[0], "ok"
            )
        finally:
            connection.close()
        manifest = (backup_dir / "manifest.sha256").read_text(encoding="ascii")
        self.assertIn("otc_bot.db", manifest)
        self.assertIn("wallet-mainnet.json", manifest)
        self.assertNotIn("network", manifest)
        second = self._run()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertNotEqual(second.stdout.strip().split(": ", 1)[1], published)
        self.assertEqual(
            (backup_dir / "manifest.sha256").read_text(encoding="ascii"), manifest
        )

    def test_traversal_symlink_hardlink_and_unsafe_mode_are_rejected(self) -> None:
        self.assertNotEqual(
            self._run(destination="/var/backups/btc09/../../tmp/escape").returncode, 0
        )
        symlink = self.root / "wallet-link.json"
        try:
            symlink.symlink_to(self.wallet)
        except OSError:
            self.skipTest("symlink creation is unavailable")
        self.assertNotEqual(self._run(wallet=symlink).returncode, 0)
        hardlink = self.root / "wallet-hard.json"
        os.link(self.wallet, hardlink)
        self.assertNotEqual(self._run(wallet=hardlink).returncode, 0)
        hardlink.unlink()
        self.assertNotEqual(self._run(unsafe=self.wallet).returncode, 0)
        self.assertNotEqual(self._run(unsafe=self.destination).returncode, 0)

    def test_generation_lock_contention_stops_before_active_state_touch(self) -> None:
        maintenance = self.root / "maintenance"
        state = self.root / "state"
        backup_root = self.root / "backups"
        destination = backup_root / "destination"
        for directory in (maintenance, state, destination):
            directory.mkdir(parents=True, exist_ok=True)
        maintenance_unix = self._unix(maintenance)
        state_unix = self._unix(state)
        backup_unix = self._unix(backup_root)
        helper_unix = self._unix(SNAPSHOT_BACKUP)
        script = self.root / "backup-contention.sh"
        text = BACKUP.read_text(encoding="utf-8")
        text = text.replace(
            "BACKUP_ROOT=/var/backups/btc09", f"BACKUP_ROOT={backup_unix}"
        )
        text = text.replace(
            "MAINTENANCE_DIR=/var/lib/btc09-otc-maintenance",
            f"MAINTENANCE_DIR={maintenance_unix}",
        )
        text = text.replace("STATE=/var/lib/btc09-otc", f"STATE={state_unix}")
        text = text.replace(
            "SNAPSHOT_HELPER=$SCRIPT_DIR/snapshot-otc-backup.py",
            f"SNAPSHOT_HELPER={helper_unix}",
        )
        script.write_text(text, encoding="utf-8")
        os.chmod(script, 0o755)
        touched = self.root / "active-state-touched"
        touched_unix = self._unix(touched)
        (self.fake / "flock").write_text(
            "#!/usr/bin/env bash\nif [[ $1 == -n && $2 == 8 ]]; then exit 1; fi\nexit 0\n",
            encoding="utf-8",
        )
        for command in ("systemctl", "chown"):
            (self.fake / command).write_text(
                f"#!/usr/bin/env bash\ntouch '{touched_unix}'\nexit 97\n",
                encoding="utf-8",
            )
        for file in (self.fake / "flock", self.fake / "systemctl", self.fake / "chown"):
            os.chmod(file, 0o755)
        env = os.environ.copy()
        env["FAKE_MAINTENANCE_PATH"] = maintenance_unix
        result = subprocess.run(
            [
                str(GIT_BASH),
                "-c",
                'export PATH="$1:$PATH"; exec "$2" "$3" "$4" "$5"',
                "bash",
                self._unix(self.fake),
                self._unix(script),
                f"{state_unix}/otc_bot.db",
                f"{state_unix}/wallet-mainnet.json",
                self._unix(destination),
            ],
            cwd=ROOT,
            env=env,
            capture_output=True,
            text=True,
            timeout=15,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("generation install", result.stderr)
        self.assertFalse(touched.exists())
        self.assertFalse((maintenance / "active").exists())
        self.assertFalse((destination / ".backup.lock").exists())


@unittest.skipIf(os.name == "nt", "POSIX dirfd and no-follow semantics required")
class BackupSnapshotRaceTests(unittest.TestCase):
    @staticmethod
    def _module():
        spec = importlib.util.spec_from_file_location(
            "otc_backup_snapshot", SNAPSHOT_BACKUP
        )
        if spec is None or spec.loader is None:
            raise AssertionError("backup snapshot helper cannot load")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.db = self.root / "otc_bot.db"
        self.wallet = self.root / "wallet-mainnet.json"
        self.destination = self.root / "destination"
        self.destination.mkdir(mode=0o700)
        _initialized_db(self.db)
        self.wallet.write_text('{"network":"btc09-mainnet"}', encoding="ascii")
        os.chmod(self.db, 0o600)
        os.chmod(self.wallet, 0o600)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def test_swapped_db_and_wallet_names_are_rejected_before_copy(self) -> None:
        module = self._module()
        for source in (self.db, self.wallet):
            with self.subTest(source=source.name):
                evil = self.root / f"evil-{source.name}"
                evil.write_bytes(b"attacker controlled")
                original = self.root / f"original-{source.name}"

                def swap() -> None:
                    source.rename(original)
                    source.symlink_to(evil)

                with self.assertRaises(module.SnapshotError):
                    module.snapshot_sources(
                        self.db,
                        self.wallet,
                        self.destination,
                        expected_uid=os.getuid(),
                        before_copy=swap,
                    )
                self.assertEqual(list(self.destination.iterdir()), [])
                source.unlink()
                original.rename(source)
                evil.unlink()

    def test_verified_sources_are_copied_through_stable_descriptors(self) -> None:
        module = self._module()
        module.snapshot_sources(
            self.db,
            self.wallet,
            self.destination,
            expected_uid=os.getuid(),
        )
        connection = sqlite3.connect(self.destination / "otc_bot.db")
        try:
            self.assertEqual(
                connection.execute("PRAGMA integrity_check").fetchone(), ("ok",)
            )
        finally:
            connection.close()
        self.assertEqual(
            (self.destination / "wallet-mainnet.json").read_bytes(),
            self.wallet.read_bytes(),
        )

    def test_preexisting_destination_symlink_is_never_followed(self) -> None:
        module = self._module()
        outside = self.root / "outside"
        outside.write_bytes(b"do not overwrite")
        (self.destination / "wallet-mainnet.json").symlink_to(outside)
        with self.assertRaises(module.SnapshotError):
            module.snapshot_sources(
                self.db,
                self.wallet,
                self.destination,
                expected_uid=os.getuid(),
            )
        self.assertEqual(outside.read_bytes(), b"do not overwrite")

    def test_failed_second_source_does_not_leak_descriptors(self) -> None:
        module = self._module()
        original = self.root / "wallet-original.json"
        self.wallet.rename(original)
        self.wallet.symlink_to(original)
        before = len(tuple(Path("/proc/self/fd").iterdir()))
        with self.assertRaises(module.SnapshotError):
            module.snapshot_sources(
                self.db,
                self.wallet,
                self.destination,
                expected_uid=os.getuid(),
            )
        after = len(tuple(Path("/proc/self/fd").iterdir()))
        self.assertEqual(after, before)


class DocumentationTests(unittest.TestCase):
    def test_runbooks_are_reproducible_and_never_copy_general_wallet(self) -> None:
        combined = BOT_README.read_text(encoding="utf-8") + DEPLOY_README.read_text(
            encoding="utf-8"
        )
        for fragment in (
            "apt-get install",
            "sqlite3",
            "btc09-otc",
            "/var/lib/btc09-otc/wallet-mainnet.json",
            "runuser -u btc09-otc",
            "migration test",
            "OTC_ACCEPTING_ORDERS=0",
            "backup-otc.sh",
            "check-otc-health.sh",
            "restore",
            "nginx -t",
            "nginx -T",
            "Cloudflare",
            "systemd-analyze security",
            "test-otc-systemd-restart.sh",
            "check-otc-process-environment.py",
            "LoadCredential",
            "btc09-otc-feed",
            "check-otc-service-isolation.sh",
            "install-otc-generation.sh",
            "MAINTENANCE",
            "Resume after reboot",
            "/var/lib/btc09-otc-public",
            "verify-otc-python-lock.py",
            "generation lock before the destination lock",
            "blocks-mainnet.dat.lock",
            "root:btc09-otc 0660",
            "single-link regular file",
            "ReadWritePaths=/opt/btc09/data/blocks-mainnet.dat.lock",
            "never grants write access to `blocks-mainnet.dat`",
            "root:btc09-otc 0640",
            "atomic replacement",
            "First creation remains mode 0600",
            "no-follow",
            "general node wallet remains `root:root 0600`",
            "REVIEWED_BTC09_SHA256",
            "systemctl stop btc09-seed",
            "systemctl start btc09-seed",
            "tip_after > tip_before",
            'blocks_identity_after != "$blocks_identity_before"',
            "strictly higher tip",
            "extended POSIX ACL",
            "install-otc-nginx.sh",
            "bitcoin09-domain-pending",
            "Certbot",
            "detailed health remains loopback-only",
        ):
            self.assertIn(fragment, combined)
        self.assertIn("Never copy `/opt/btc09/data/wallet-mainnet.json`", combined)
        self.assertNotIn("/etc/btc09/discord.env", combined)
        self.assertNotIn("rm -f /var/lib/btc09-otc/otc_bot.db", combined)
        self.assertNotIn("/var/lib/btc09-otc/public", combined)


class NginxModularDeploymentTests(unittest.TestCase):
    @staticmethod
    def _patch_module():
        spec = importlib.util.spec_from_file_location("otc_nginx_patch", PATCH_NGINX)
        if spec is None or spec.loader is None:
            raise AssertionError("nginx patch helper cannot load")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_http_fragment_has_exact_cloudflare_ranges_and_shared_zones(self) -> None:
        text = NGINX_HTTP.read_text(encoding="ascii")
        ranges = tuple(
            line.removeprefix("set_real_ip_from ").removesuffix(";")
            for line in text.splitlines()
            if line.startswith("set_real_ip_from ")
        )
        self.assertEqual(
            ranges,
            (
                "173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22",
                "103.31.4.0/22", "141.101.64.0/18", "108.162.192.0/18",
                "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22",
                "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
                "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
                "2400:cb00::/32", "2606:4700::/32", "2803:f800::/32",
                "2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29",
                "2c0f:f248::/32",
            ),
        )
        self.assertIn("real_ip_header CF-Connecting-IP;", text)
        self.assertIn("real_ip_recursive on;", text)
        self.assertIn("limit_req_zone $binary_remote_addr zone=otc_feed_rate:10m rate=5r/s;", text)
        self.assertIn("limit_conn_zone $binary_remote_addr zone=otc_feed_conn:10m;", text)
        self.assertNotIn("server {", text)

    def test_server_snippet_exposes_only_feed_with_bounded_proxying(self) -> None:
        text = NGINX_SERVER.read_text(encoding="ascii")
        self.assertIn("location = /otc-bot-feed.json {", text)
        self.assertIn("proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;", text)
        self.assertIn("limit_req zone=otc_feed_rate burst=10 nodelay;", text)
        self.assertIn("limit_conn otc_feed_conn 4;", text)
        for fragment in ("proxy_connect_timeout", "proxy_send_timeout", "proxy_read_timeout"):
            self.assertIn(fragment, text)
        for forbidden in (
            "healthz",
            "proxy_cache",
            "proxy_hide_header",
            "add_header",
        ):
            self.assertNotIn(forbidden, text)

    def test_tls_site_patch_is_idempotent_and_preserves_certbot_and_explorer(self) -> None:
        module = self._patch_module()
        source = """
server {
    server_name btc09.org www.btc09.org;
    listen 443 ssl; # managed by Certbot
    ssl_certificate /etc/letsencrypt/live/btc09.org/fullchain.pem;
    location = /otc-bot-feed.json {
        proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;
        add_header Access-Control-Allow-Origin *;
    }
    location = /otc-feed-healthz { proxy_pass http://127.0.0.1:8019/healthz; }
}
server {
    server_name www.btc09.org;
    listen 443 ssl;
    location = /otc-bot-feed.json { proxy_pass http://127.0.0.1:8019/otc-bot-feed.json; }
    return 301 https://btc09.org$request_uri;
}
server {
    server_name explorer.btc09.org;
    listen 443 ssl;
    location / { proxy_pass http://127.0.0.1:8009; }
}
""".lstrip()
        patched = module.transform_site(source)
        self.assertEqual(patched.count("include /etc/nginx/snippets/bitcoin09-otc-server.conf;"), 1)
        self.assertNotIn("location = /otc-bot-feed.json", patched)
        self.assertNotIn("otc-feed-healthz", patched)
        for preserved in (
            "listen 443 ssl; # managed by Certbot",
            "ssl_certificate /etc/letsencrypt/live/btc09.org/fullchain.pem;",
            "location / { proxy_pass http://127.0.0.1:8009; }",
            "return 301 https://btc09.org$request_uri;",
        ):
            self.assertIn(preserved, patched)
        self.assertEqual(module.transform_site(patched), patched)

    def test_tls_site_patch_rejects_health_and_ambiguous_targets(self) -> None:
        module = self._patch_module()
        for health_path in ("/healthz", "/other-healthz", "/otc-feed-healthz-alias"):
            with self.subTest(health_path=health_path), self.assertRaises(module.NginxPatchError):
                module.transform_site(
                    f"server {{ listen 443 ssl; server_name btc09.org; location = {health_path} {{}} }}"
                )
        with self.assertRaises(module.NginxPatchError):
            module.transform_site(
                "server { listen 443 ssl; server_name btc09.org; "
                "location ~ ^/private-healthz(?:/|$) {} }"
            )
        other_vhost_health = module.transform_site(
            "server { listen 443 ssl; server_name btc09.org; }\n"
            "server { listen 443 ssl; server_name www.btc09.org; "
            "location = /otc-feed-healthz {} }"
        )
        self.assertIn("location = /otc-feed-healthz {}", other_vhost_health)
        ambiguous = "\n".join(
            "server { listen 443 ssl; server_name btc09.org; }" for _ in range(2)
        )
        with self.assertRaises(module.NginxPatchError):
            module.transform_site(ambiguous)

    def test_tls_site_patch_rejects_alternate_upstreams(self) -> None:
        module = self._patch_module()
        base = "server { listen 443 ssl; server_name btc09.org; %s }"
        bypasses = (
            "location = /alternate { proxy_pass http://127.0.0.1:8019/otc-bot-feed.json; }",
            "location /nested { location /feed { proxy_pass http://127.0.0.1:8019; } }",
            "include /etc/nginx/snippets/unreviewed-8019.conf;",
            "location /dynamic { set $port_a 80; set $port_b 19; "
            "proxy_pass http://127.0.0.1:$port_a$port_b/healthz; }",
            "location @named_otc { proxy_pass http://otc_dynamic; }",
            "location /outer { location /inner { "
            "proxy_pass http://otc_dynamic; } }",
        )
        for bypass in bypasses:
            with self.subTest(bypass=bypass), self.assertRaises(module.NginxPatchError):
                module.transform_site(base % bypass)

    def test_tls_site_patch_preserves_headers_owned_by_sibling_locations(self) -> None:
        module = self._patch_module()
        source = """
server {
  listen 443 ssl;
  server_name btc09.org;
  location /assets/ {
    add_header Cache-Control public;
    add_header Access-Control-Allow-Origin https://static.btc09.org;
  }
}
"""
        patched = module.transform_site(source)
        self.assertIn("add_header Cache-Control public;", patched)
        self.assertIn(
            "add_header Access-Control-Allow-Origin https://static.btc09.org;",
            patched,
        )

    def test_tls_site_patch_normalizes_obfuscated_names_before_auditing(self) -> None:
        module = self._patch_module()
        base = "server { listen 443 ssl; server_name btc09.org; %s }"
        for bypass in (
            "location /dynamic { set $port_a 80; set $port_b 19; "
            '"proxy_pass" http://127.0.0.1:$port_a$port_b/healthz; }',
            r"location /escaped { proxy\_pass http://otc_dynamic; }",
            "include /etc/nginx/snippets/bitcoin09-otc-server.conf; "
            '"include" /etc/nginx/snippets/bitcoin09-otc-server.conf;',
            "location /hash-split { set $a 80; set $b 19; "
            "set $pad harmless#literal; "
            "proxy_pass http://127.0.0.1:$a$b/healthz; }",
            "location /braced-split { set $a 80; set $b 19; "
            "proxy_pass http://127.0.0.1:${a}${b}/healthz; }",
        ):
            with self.subTest(bypass=bypass), self.assertRaises(module.NginxPatchError):
                module.transform_site(base % bypass)

    def test_nginx_value_normalization_preserves_unrelated_subgrammars(self) -> None:
        module = self._patch_module()
        subgrammars = """
map $http_x_test $mapped_test { default off; '' empty; }
split_clients $request_id $bucket_test { "10%" first; * other; }
geo $trusted_test { "127.0.0.1" 1; default 0; }
"""
        source = (
            subgrammars
            + "server { listen 443 ssl; server_name btc09.org; "
            "set $display_test ${mapped_test}:${bucket_test}; }"
        )
        patched = module.transform_site(source)
        self.assertIn(subgrammars, patched)
        self.assertIn("set $display_test ${mapped_test}:${bucket_test};", patched)
        effective = """
http {
  server { listen 443 ssl; server_name btc09.org;
    set $display_test ${mapped_test}:${bucket_test};
    include /etc/nginx/snippets/bitcoin09-otc-server.conf;
  }
  server { listen 443 ssl; server_name explorer.btc09.org;
    location / { proxy_pass http://127.0.0.1:8009; }
  }
}
location = /otc-bot-feed.json {
  proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;
}
"""
        module.audit_effective_config(effective + subgrammars)
        hash_tokens = module._tokens("set $pad harmless#literal;")
        self.assertIn("harmless#literal", tuple(token.value for token in hash_tokens))
        braced_tokens = module._tokens(
            "set $display_test ${mapped_test}:${bucket_test};"
        )
        self.assertIn(
            "${mapped_test}:${bucket_test}",
            tuple(token.value for token in braced_tokens),
        )
        for invalid in ("set $x ${missing;", "set $x ${bad-name};"):
            with self.subTest(invalid=invalid), self.assertRaises(
                module.NginxPatchError
            ):
                module._tokens(invalid)

    def test_effective_config_and_tls_headers_are_strictly_audited(self) -> None:
        module = self._patch_module()
        effective = """
http {
  server { listen 443 ssl; server_name btc09.org;
    include /etc/nginx/snippets/bitcoin09-otc-server.conf;
  }
  server { listen 443 ssl; server_name explorer.btc09.org;
    location / { proxy_pass http://127.0.0.1:8009; }
  }
}
# nginx -T prints included file bodies separately.
location = /otc-bot-feed.json {
  proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;
}
"""
        module.audit_effective_config(effective)
        for addition in (
            "proxy_pass http://127.0.0.1:8019/healthz;",
            "proxy_pass http://127.0.0.1:8019/alternate;",
            "upstream hidden { server 127.0.0.1:8019; }",
            "upstream hidden { server localhost:8019; }",
            "upstream hidden { server [::1]:8019; }",
            "upstream hidden { server [::ffff:127.0.0.1]:8019; }",
            "set $otc_port 8019; proxy_pass http://hidden:$otc_port;",
            "set $prefixed_port 18019;",
            "include /etc/nginx/snippets/bitcoin09-otc-server.conf;",
        ):
            with self.subTest(addition=addition), self.assertRaises(module.NginxPatchError):
                module.audit_effective_config(effective + addition)
        for feed_header_override in (
            "proxy_hide_header Access-Control-Allow-Origin;",
            "proxy_hide_header Cache-Control;",
            'add_header Access-Control-Allow-Origin "*" always;',
            'add_header Cache-Control "no-store" always;',
            "add_header X-Content-Type-Options nosniff always;",
        ):
            with self.subTest(
                feed_header_override=feed_header_override
            ), self.assertRaises(module.NginxPatchError):
                module.audit_effective_config(
                    effective.replace(
                        "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;",
                        "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json; "
                        + feed_header_override,
                    )
                )
        module.audit_effective_config(
            effective
            + "server { listen 443 ssl; server_name unrelated.invalid; "
            "location = /healthz { return 204; } add_header Cache-Control public; }"
        )
        for dynamic_proxy in (
            "location /dynamic { set $port_a 80; set $port_b 19; "
            "proxy_pass http://127.0.0.1:$port_a$port_b/healthz; }",
            "location @named_otc { proxy_pass http://otc_dynamic; }",
            "location /outer { location /inner { "
            "proxy_pass http://otc_dynamic; } }",
            "location /hash-split { set $a 80; set $b 19; "
            "set $pad harmless#literal; "
            "proxy_pass http://127.0.0.1:$a$b/healthz; }",
            "location /braced-split { set $a 80; set $b 19; "
            "proxy_pass http://127.0.0.1:${a}${b}/healthz; }",
        ):
            with self.subTest(dynamic_proxy=dynamic_proxy), self.assertRaises(
                module.NginxPatchError
            ):
                module.audit_effective_config(
                    effective.replace(
                        "include /etc/nginx/snippets/bitcoin09-otc-server.conf;",
                        "include /etc/nginx/snippets/bitcoin09-otc-server.conf; "
                        + dynamic_proxy,
                    )
                )
        with self.assertRaises(module.NginxPatchError):
            module.audit_effective_config(
                effective
                + "# nginx -T body from an opaque included file\n"
                "set $opaque_a 80; set $opaque_b 19; "
                "location /opaque { proxy_pass "
                "http://127.0.0.1:$opaque_a$opaque_b/feed; }"
            )
        for opaque_directive in (
            "location /quoted-split { set $a 80; set $b 19; "
            '"proxy_pass" http://127.0.0.1:$a$b/healthz; }',
            "location /quoted-split-safe-path { set $a 80; set $b 19; "
            '"proxy_pass" http://127.0.0.1:$a$b/alternate; }',
            '"include" /etc/nginx/snippets/bitcoin09-otc-server.conf;',
            r"proxy\_pass http://otc_dynamic;",
        ):
            with self.subTest(opaque_directive=opaque_directive), self.assertRaises(
                module.NginxPatchError
            ):
                module.audit_effective_config(effective + opaque_directive)
        for obfuscated_header in (
            '"add_header" Cache-Control public;',
            r"add\_header Cache-Control public;",
        ):
            with self.subTest(obfuscated_header=obfuscated_header), self.assertRaises(
                module.NginxPatchError
            ):
                module.audit_effective_config(
                    effective.replace(
                        "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;",
                        "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json; "
                        + obfuscated_header,
                    )
                )
        for opaque_proxy in (
            "location /opaque { proxy_pass http://otc_dynamic; }",
            "location /opaque { location /nested { "
            "proxy_pass http://otc_dynamic; } }",
            "location @opaque_named { proxy_pass http://127.0.0.1:8008; }",
        ):
            with self.subTest(opaque_proxy=opaque_proxy), self.assertRaises(
                module.NginxPatchError
            ):
                module.audit_effective_config(effective + opaque_proxy)
        for invalid_multiset in (
            effective.replace(
                "location / { proxy_pass http://127.0.0.1:8009; }", ""
            ),
            effective
            + "server { listen 443 ssl; server_name explorer-copy.invalid; "
            "location / { proxy_pass http://127.0.0.1:8009; } }",
            effective.replace(
                "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;", ""
            ),
            effective.replace(
                "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;",
                "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json; "
                "proxy_pass http://127.0.0.1:8019/otc-bot-feed.json;",
            ),
        ):
            with self.subTest(invalid_multiset=invalid_multiset), self.assertRaises(
                module.NginxPatchError
            ):
                module.audit_effective_config(invalid_multiset)
        module.audit_tls_headers(
            "HTTP/2 200\r\nAccess-Control-Allow-Origin: *\r\n"
            "Cache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\n\r\n"
        )
        with self.assertRaises(module.NginxPatchError):
            module.audit_tls_headers(
                "HTTP/2 200\r\nAccess-Control-Allow-Origin: *\r\n"
                "Access-Control-Allow-Origin: *\r\nCache-Control: no-store\r\n\r\n"
            )
        with self.assertRaises(module.NginxPatchError):
            module.audit_tls_headers(
                "HTTP/1.1 100 Continue\r\n\r\nHTTP/2 200\r\n"
                "Access-Control-Allow-Origin: *\r\nCache-Control: no-store\r\n"
                "X-Content-Type-Options: nosniff\r\n\r\n"
            )

    def test_installer_is_exact_backup_validated_and_rollback_capable(self) -> None:
        text = INSTALL_NGINX.read_text(encoding="utf-8")
        for fragment in (
            "/etc/nginx/sites-enabled/bitcoin09-domain-pending",
            "/etc/nginx/sites-available/bitcoin09-domain-pending",
            "/etc/nginx/conf.d/bitcoin09-otc-http.conf",
            "/etc/nginx/snippets/bitcoin09-otc-server.conf",
            "/var/backups/btc09",
            "sha256sum",
            "nginx -t",
            "systemctl reload nginx",
            "rollback",
            "patch-otc-nginx-site.py",
            '[[ -L $ENABLED_SITE ]]',
            'realpath -e -- "$ENABLED_SITE"',
            "flock -n 9",
            "baseline_matches",
            "installed_matches",
            "sha256sum --check --strict",
            "audit-effective",
            "audit-headers",
            "--resolve btc09.org:443:127.0.0.1",
            "--noproxy '*'",
            "otc-feed-healthz",
            "CRITICAL: OTC nginx rollback failed",
            'desired_http_identity=$(stat -Lc \'%d:%i\' -- "$http_tmp")',
            'baseline_entry_matches "$name" && return 0',
            "/etc/nginx/.certbot.lock",
            "with-certbot-nginx-lock.py",
            "OTC_NGINX_CERTBOT_LOCK_HELD",
            'sync "$backup/BASELINE"',
            'sync "$backup/SHA256SUMS"',
            "sync_restored_present_files",
            "wait-otc-nginx-readback.py",
            'python3 "$READBACK_HELPER" "$PATCH_HELPER"',
            "--no-keepalive",
            "Connection: close",
        ):
            self.assertIn(fragment, text)
        self.assertNotIn("http_moved", text)
        self.assertNotIn("server_moved", text)
        self.assertNotIn("site_moved", text)
        self.assertLess(
            text.index('python3 "$READBACK_HELPER" "$PATCH_HELPER"'),
            text.index('python3 "$PATCH_HELPER" --audit-headers'),
        )
        for rollback_fragment in (
            "backup_valid || return 1",
            "installed_matches || return 1",
            "restore_entry site || return 1",
            "restore_entry http || return 1",
            "restore_entry server || return 1",
            "restored_matches_baseline || return 1",
            "nginx -t || return 1",
            "systemctl reload nginx || return 1",
            "trap on_exit EXIT",
            "if ((status != 0 && modified)); then",
        ):
            self.assertIn(rollback_fragment, text)

    def test_installer_orders_drift_and_rollback_gates_before_mutation_reload(self) -> None:
        text = INSTALL_NGINX.read_text(encoding="utf-8")
        self.assertLess(
            text.index('exec python3 "$CERTBOT_LOCK_HELPER" "$INSTALLER" "$@"'),
            text.index('exec 9>"$INSTALL_LOCK"'),
        )
        backup_sync = text.split("backup_files=(BASELINE site)", 1)[1].split(
            "baseline_entry_matches()", 1
        )[0]
        for present_backup in ('sync "$backup/$name"', 'sync "$backup/BASELINE"'):
            self.assertIn(present_backup, backup_sync)
        self.assertLess(
            backup_sync.index('sync "$backup/SHA256SUMS"'),
            backup_sync.index('sync "$backup"'),
        )
        self.assertLess(
            backup_sync.index('sync "$backup"'),
            backup_sync.index('sync "$BACKUP_ROOT"'),
        )
        install_flow = text.split(
            "# Revalidate the complete baseline immediately before the first replacement.",
            1,
        )[1]
        self.assertLess(
            install_flow.index('baseline_matches || fail "nginx baseline drifted'),
            install_flow.index('mv -Tf -- "$http_tmp"'),
        )
        self.assertLess(
            install_flow.index('python3 "$PATCH_HELPER" --audit-effective'),
            install_flow.index("systemctl reload nginx"),
        )
        rollback = text.split("rollback() {", 1)[1].split("\n}\n\non_exit()", 1)[0]
        ordered = (
            "backup_valid || return 1",
            "installed_matches || return 1",
            "restore_entry site || return 1",
            "sync_restored_present_files || return 1",
            "restored_matches_baseline || return 1",
            "nginx -t || return 1",
            "systemctl reload nginx || return 1",
        )
        positions = tuple(rollback.index(fragment) for fragment in ordered)
        self.assertEqual(positions, tuple(sorted(positions)))
        self.assertNotIn("|| true", rollback)


class CertbotNginxLockTests(unittest.TestCase):
    @staticmethod
    def _linux_path(path: Path) -> str:
        if os.name != "nt":
            return str(path)
        drive, tail = os.path.splitdrive(str(path))
        return f"/mnt/{drive[0].lower()}{tail.replace(os.sep, '/')}"

    def _run_linux(self, source: str) -> subprocess.CompletedProcess[str]:
        command = [sys.executable, "-c", source, self._linux_path(CERTBOT_NGINX_LOCK)]
        if os.name == "nt":
            command = ["wsl.exe", "--", "python3", "-c", source, command[-1]]
        try:
            return subprocess.run(
                command,
                cwd=ROOT,
                capture_output=True,
                text=True,
                timeout=15,
            )
        except FileNotFoundError:
            self.skipTest("a Linux Python runtime is unavailable")

    def test_certbot_lock_contends_across_processes_and_unlinks_on_release(
        self,
    ) -> None:
        source = textwrap.dedent(
            """
            import importlib.util
            import os
            from pathlib import Path
            import subprocess
            import sys
            import tempfile

            helper = Path(sys.argv[1])
            spec = importlib.util.spec_from_file_location("certbot_lock", helper)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            with tempfile.TemporaryDirectory() as directory:
                lock_path = Path(directory) / ".certbot.lock"
                holder_source = "\\n".join((
                    "import importlib.util, os, pathlib, sys, time",
                    'spec = importlib.util.spec_from_file_location("holder_lock", pathlib.Path(sys.argv[1]))',
                    "module = importlib.util.module_from_spec(spec)",
                    "spec.loader.exec_module(module)",
                    "with module.VerifiedCertbotLock(pathlib.Path(sys.argv[2]), os.geteuid()):",
                    '    print("held", flush=True)',
                    "    time.sleep(1)",
                ))
                holder = subprocess.Popen(
                    [sys.executable, "-c", holder_source, str(helper), str(lock_path)],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                    text=True,
                )
                assert holder.stdout.readline().strip() == "held"
                try:
                    with module.VerifiedCertbotLock(lock_path, os.geteuid()):
                        raise AssertionError("a contended lock was acquired")
                except module.CertbotLockError:
                    pass
                assert holder.wait(timeout=5) == 0, holder.stderr.read()
                assert not lock_path.exists()
            """
        )
        result = self._run_linux(source)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_certbot_lock_retries_when_path_inode_changes_during_acquire(self) -> None:
        source = textwrap.dedent(
            """
            import importlib.util
            import os
            from pathlib import Path
            import sys
            import tempfile

            helper = Path(sys.argv[1])
            spec = importlib.util.spec_from_file_location("certbot_lock_race", helper)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            with tempfile.TemporaryDirectory() as directory:
                lock_path = Path(directory) / ".certbot.lock"
                real_lstat = module.os.lstat
                raced = False
                def racing_lstat(path, *args, **kwargs):
                    global raced
                    if not raced and Path(path) == lock_path and not args and not kwargs:
                        raced = True
                        os.unlink(path)
                        Path(path).touch(mode=0o600)
                    return real_lstat(path, *args, **kwargs)
                module.os.lstat = racing_lstat
                with module.VerifiedCertbotLock(lock_path, os.geteuid()):
                    assert raced
                assert not lock_path.exists()
            """
        )
        result = self._run_linux(source)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_certbot_lock_cli_is_pinned_to_certbot_path_and_nonblocking_lockf(
        self,
    ) -> None:
        text = CERTBOT_NGINX_LOCK.read_text(encoding="utf-8")
        for fragment in (
            'CERTBOT_LOCK_PATH = Path("/etc/nginx/.certbot.lock")',
            "os.O_CREAT | os.O_WRONLY",
            "fcntl.LOCK_EX | fcntl.LOCK_NB",
            "os.fchmod",
            "os.lstat",
            "os.unlink",
            "OTC_NGINX_CERTBOT_LOCK_HELD",
        ):
            self.assertIn(fragment, text)


class NginxReadinessTests(unittest.TestCase):
    @staticmethod
    def _module():
        spec = importlib.util.spec_from_file_location(
            "otc_nginx_readiness", NGINX_READINESS
        )
        if spec is None or spec.loader is None:
            raise AssertionError("nginx readiness helper cannot load")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def _wait(self, sequence: list[bool]) -> tuple[bool, int]:
        module = self._module()
        now = [0.0]
        calls = 0

        def probe(_remaining: float) -> bool:
            nonlocal calls
            result = sequence[calls] if calls < len(sequence) else False
            calls += 1
            return result

        result = module.wait_until_ready(
            probe,
            timeout=2.0,
            consecutive=3,
            interval=0.25,
            clock=lambda: now[0],
            pause=lambda seconds: now.__setitem__(0, now[0] + seconds),
        )
        return result, calls

    def test_old_workers_then_three_new_worker_signatures_succeed(self) -> None:
        self.assertEqual(self._wait([False, False, True, True, True]), (True, 5))

    def test_intermittent_old_worker_resets_consecutive_successes(self) -> None:
        self.assertEqual(
            self._wait([True, True, False, True, True, True]),
            (True, 6),
        )

    def test_never_new_worker_times_out_and_installer_fails_closed(self) -> None:
        ready, calls = self._wait([False] * 20)
        self.assertFalse(ready)
        self.assertGreaterEqual(calls, 2)
        installer = INSTALL_NGINX.read_text(encoding="utf-8")
        self.assertIn(
            'python3 "$READBACK_HELPER" "$PATCH_HELPER" || '
            'fail "new nginx workers did not become ready"',
            installer.replace("\\\n  ", ""),
        )

    def test_live_probe_contract_is_fresh_bounded_and_complete(self) -> None:
        source = NGINX_READINESS.read_text(encoding="utf-8")
        for fragment in (
            "READINESS_TIMEOUT_SECONDS = 10.0",
            "READINESS_CONSECUTIVE_SUCCESSES = 3",
            "time.monotonic",
            '"--noproxy"',
            '"--no-keepalive"',
            '"Connection: close"',
            "audit_headers",
            "https://btc09.org/otc-bot-feed.json",
            "https://btc09.org/otc-feed-healthz",
            'health.stdout != "404"',
        ):
            self.assertIn(fragment, source)

    def test_live_probe_records_bounded_failure_category(self) -> None:
        module = self._module()
        with tempfile.TemporaryDirectory() as directory:
            probe = module.LocalTlsProbe(
                curl="curl",
                headers_path=Path(directory) / "headers",
                audit_headers=lambda _headers: None,
                clock=lambda: 0.0,
            )
            probe._run = MagicMock(
                return_value=subprocess.CompletedProcess([], 7, "", "connection failed")
            )

            self.assertFalse(probe(1.0))
            self.assertEqual(probe.last_failure, "feed transport")

    def test_partial_success_timeout_reports_incomplete_streak(self) -> None:
        module = self._module()
        now = [0.0]
        outcomes = iter((False, True, True))

        class Probe:
            last_failure: str | None = "feed headers"

            def __call__(self, _remaining: float) -> bool:
                outcome = next(outcomes)
                self.last_failure = None if outcome else "feed headers"
                return outcome

        probe = Probe()
        ready = module.wait_until_ready(
            probe,
            timeout=0.75,
            consecutive=3,
            interval=0.25,
            clock=lambda: now[0],
            pause=lambda seconds: now.__setitem__(0, now[0] + seconds),
        )

        self.assertFalse(ready)
        self.assertEqual(
            module.readiness_failure_reason(probe), "readiness streak timeout"
        )


class CredentialBoundaryTests(unittest.TestCase):
    def _credential(self, content: bytes) -> str:
        directory = tempfile.mkdtemp()
        path = Path(directory) / "otc-secrets"
        path.write_bytes(content)
        os.chmod(path, 0o600)
        self.addCleanup(lambda: Path(directory).rmdir())
        self.addCleanup(path.unlink)
        return str(path)

    @staticmethod
    def _systemd_metadata(
        payload: bytes,
        *,
        file_mode: int = 0o440,
        file_uid: int = 0,
        file_gid: int = 0,
        file_nlink: int = 1,
        file_type: int = stat.S_IFREG,
        directory_mode: int = 0o550,
        directory_uid: int = 0,
        directory_gid: int = 0,
        directory_device: int = 45,
        parent_uid: int = 0,
        parent_gid: int = 0,
        parent_device: int = 24,
    ) -> tuple[MagicMock, MagicMock, MagicMock]:
        file_metadata = MagicMock(
            st_mode=file_type | file_mode,
            st_nlink=file_nlink,
            st_uid=file_uid,
            st_gid=file_gid,
            st_dev=directory_device,
            st_ino=2,
            st_size=len(payload),
            st_mtime_ns=1,
        )
        directory_metadata = MagicMock(
            st_mode=stat.S_IFDIR | directory_mode,
            st_nlink=2,
            st_uid=directory_uid,
            st_gid=directory_gid,
            st_dev=directory_device,
            st_ino=1,
        )
        parent_metadata = MagicMock(
            st_mode=stat.S_IFDIR | 0o755,
            st_nlink=2,
            st_uid=parent_uid,
            st_gid=parent_gid,
            st_dev=parent_device,
            st_ino=1,
        )
        return file_metadata, directory_metadata, parent_metadata

    def test_exact_systemd_0440_credential_copy_loads_through_config(self) -> None:
        from bot.btc09_otc_bot import Config

        payload = b"BOT_TOKEN=systemd-secret\n"
        path = "/run/credentials/btc09-otc-bot.service/otc-secrets"
        directory = "/run/credentials/btc09-otc-bot.service"
        metadata, directory_metadata, parent_metadata = self._systemd_metadata(payload)

        def lstat(candidate: str) -> MagicMock:
            try:
                return {
                    path: metadata,
                    directory: directory_metadata,
                    "/run/credentials": parent_metadata,
                }[candidate]
            except KeyError:
                raise FileNotFoundError(candidate) from None

        with (
            patch("bot.btc09_otc_bot.os.name", "posix"),
            patch("bot.btc09_otc_bot.os.lstat", side_effect=lstat),
            patch("bot.btc09_otc_bot.os.open", return_value=7),
            patch("bot.btc09_otc_bot.os.fstat", return_value=metadata),
            patch("bot.btc09_otc_bot.os.read", return_value=payload),
            patch("bot.btc09_otc_bot.os.close"),
        ):
            config = Config.from_environment(
                {
                    "OTC_SECRETS_FILE": path,
                    "CREDENTIALS_DIRECTORY": directory,
                }
            )
        self.assertEqual(config.token, "systemd-secret")
        self.assertNotIn("systemd-secret", repr(config))

    def test_systemd_0440_copy_rejects_unbound_path_and_unsafe_metadata(self) -> None:
        from bot.btc09_otc_bot import load_otc_secrets

        payload = b"BOT_TOKEN=systemd-secret\n"
        path = "/run/credentials/btc09-otc-bot.service/otc-secrets"
        directory = "/run/credentials/btc09-otc-bot.service"
        safe = self._systemd_metadata(payload)
        cases = (
            ("direct_0440", self._systemd_metadata(payload), None),
            ("wrong_directory", safe, "/run/credentials/other.service"),
            ("wrong_uid", self._systemd_metadata(payload, file_uid=1), directory),
            ("wrong_gid", self._systemd_metadata(payload, file_gid=1), directory),
            ("wrong_mode", self._systemd_metadata(payload, file_mode=0o640), directory),
            (
                "hard_link",
                self._systemd_metadata(payload, file_nlink=2),
                directory,
            ),
            (
                "symlink",
                self._systemd_metadata(payload, file_type=stat.S_IFLNK),
                directory,
            ),
            (
                "wrong_directory_mode",
                self._systemd_metadata(payload, directory_mode=0o750),
                directory,
            ),
            (
                "wrong_directory_owner",
                self._systemd_metadata(payload, directory_uid=1),
                directory,
            ),
            (
                "wrong_parent_owner",
                self._systemd_metadata(payload, parent_gid=1),
                directory,
            ),
            (
                "same_device_not_mount",
                self._systemd_metadata(payload, parent_device=45),
                directory,
            ),
        )
        for name, metadata_set, credential_directory in cases:
            metadata, directory_metadata, parent_metadata = metadata_set

            def lstat(candidate: str) -> MagicMock:
                try:
                    return {
                        path: metadata,
                        directory: directory_metadata,
                        "/run/credentials": parent_metadata,
                    }[candidate]
                except KeyError:
                    raise FileNotFoundError(candidate) from None

            with (
                self.subTest(name=name),
                patch("bot.btc09_otc_bot.os.name", "posix"),
                patch("bot.btc09_otc_bot.os.lstat", side_effect=lstat),
                patch("bot.btc09_otc_bot.os.open", return_value=7),
                patch("bot.btc09_otc_bot.os.fstat", return_value=metadata),
                patch("bot.btc09_otc_bot.os.read", return_value=payload),
                patch("bot.btc09_otc_bot.os.close"),
                self.assertRaisesRegex(ValueError, "credential"),
            ):
                load_otc_secrets(
                    path,
                    credential_directory=credential_directory,
                )

    def test_systemd_credential_relative_and_traversal_paths_are_rejected(self) -> None:
        from bot.btc09_otc_bot import load_otc_secrets

        path = "/run/credentials/btc09-otc-bot.service/otc-secrets"
        directory = "/run/credentials/btc09-otc-bot.service"
        for candidate, credential_directory in (
            ("otc-secrets", directory),
            (
                "/run/credentials/btc09-otc-bot.service/../btc09-otc-bot.service/otc-secrets",
                directory,
            ),
            (path, "/run/credentials/../credentials/btc09-otc.service"),
            ("/tmp/btc09-otc.service/otc-secrets", "/tmp/btc09-otc.service"),
        ):
            with (
                self.subTest(candidate=candidate),
                self.assertRaisesRegex(ValueError, "credential"),
            ):
                load_otc_secrets(
                    str(candidate),
                    credential_directory=credential_directory,
                )

    def test_allowlisted_credentials_load_without_mutating_process_environment(
        self,
    ) -> None:
        from bot.btc09_otc_bot import Config

        path = self._credential(
            b"BOT_TOKEN=test-token\nDISCORD_GUILD_ID=123\nADMIN_IDS=4,5\n"
            b"TRANSLATION_API_URL=https://translation.invalid/v1\n"
            b"TRANSLATION_API_TOKEN=translation-secret\n"
            b"OTC_ADMIN_FEE_DESTINATION=admin-address\n"
        )
        before = dict(os.environ)
        config = Config.from_environment(
            {
                "OTC_SECRETS_FILE": path,
                "OTC_ACCEPTING_ORDERS": "1",
                "DB_PATH": "/var/lib/btc09-otc/otc_bot.db",
                "BOT_TOKEN": "environment-token-must-be-ignored",
            }
        )
        self.assertEqual(config.token, "test-token")
        self.assertEqual(config.guild_id, 123)
        self.assertEqual(config.admin_ids, frozenset({4, 5}))
        self.assertEqual(config.translation_api_url, "https://translation.invalid/v1")
        self.assertEqual(config.translation_api_token, "translation-secret")
        self.assertEqual(config.db_path, "/var/lib/btc09-otc/otc_bot.db")
        self.assertNotIn("test-token", repr(config))
        self.assertNotIn("translation-secret", repr(config))
        self.assertNotIn("admin-address", repr(config))
        self.assertEqual(dict(os.environ), before)

    def test_direct_environment_secrets_are_ignored_without_a_credential(self) -> None:
        from bot.btc09_otc_bot import Config

        config = Config.from_environment(
            {
                "BOT_TOKEN": "direct-token-must-not-load",
                "DISCORD_GUILD_ID": "999",
                "ADMIN_IDS": "123",
                "TRANSLATION_API_TOKEN": "direct-translation-secret",
                "OTC_ADMIN_FEE_DESTINATION": "direct-admin-destination",
            }
        )
        self.assertEqual(config.token, "")
        self.assertEqual(config.guild_id, 0)
        self.assertEqual(config.admin_ids, frozenset())
        self.assertEqual(config.translation_api_token, "")
        self.assertIsNone(config.admin_fee_destination)

    def test_runtime_passes_only_loaded_translation_mapping_to_provider(self) -> None:
        from bot.btc09_otc_bot import Config, build_runtime

        path = self._credential(
            b"BOT_TOKEN=test-token\n"
            b"TRANSLATION_API_URL=https://translation.invalid/v1\n"
            b"TRANSLATION_API_TOKEN=translation-secret\n"
        )
        with tempfile.TemporaryDirectory() as directory:
            config = Config.from_environment(
                {
                    "OTC_SECRETS_FILE": path,
                    "DB_PATH": str(Path(directory) / "otc.db"),
                }
            )
            with (
                patch("bot.btc09_otc_bot.Store") as store_class,
                patch("bot.btc09_otc_bot.Explorer") as explorer_class,
                patch("bot.btc09_otc_bot.Wallet") as wallet_class,
                patch("bot.btc09_otc_bot.TradeService") as service_class,
                patch("bot.btc09_otc_bot.DiscordTradeUI") as ui_class,
                patch(
                    "bot.btc09_otc_bot.translation_provider_from_environment"
                ) as provider_factory,
            ):
                store_class.return_value.initialize.return_value = None
                wallet_class.return_value.new_address = MagicMock()
                runtime = build_runtime(config)
        provider_factory.assert_called_once_with(
            {
                "TRANSLATION_API_URL": "https://translation.invalid/v1",
                "TRANSLATION_API_TOKEN": "translation-secret",
            }
        )
        self.assertIs(runtime.service, service_class.return_value)
        self.assertIs(runtime.controller, ui_class.return_value)
        self.assertTrue(explorer_class.called)

    def test_credentials_cannot_override_safety_critical_configuration(self) -> None:
        from bot.btc09_otc_bot import Config

        for line in (
            b"OTC_ACCEPTING_ORDERS=1\n",
            b"DB_PATH=/tmp/attacker.db\n",
            b"BTC09_NETWORK=btc09-regtest\n",
        ):
            with self.subTest(line=line):
                path = self._credential(b"BOT_TOKEN=test-token\n" + line)
                with self.assertRaisesRegex(ValueError, "credential"):
                    Config.from_environment({"OTC_SECRETS_FILE": path})

    def test_duplicate_unknown_malformed_and_oversized_credentials_fail_closed(
        self,
    ) -> None:
        from bot.btc09_otc_bot import Config

        cases = (
            b"BOT_TOKEN=first\nBOT_TOKEN=second\n",
            b"BOT_TOKEN=test\nUNKNOWN_SECRET=value\n",
            b"BOT_TOKEN\n",
            b" BOT_TOKEN=test\n",
            b"BOT_TOKEN=test\x00tail\n",
            b"BOT_TOKEN=" + b"x" * 20_000 + b"\n",
            b"BOT_TOKEN=test\nDISCORD_BOT_TOKEN=second\n",
        )
        for content in cases:
            with self.subTest(size=len(content)):
                path = self._credential(content)
                with self.assertRaisesRegex(ValueError, "credential"):
                    Config.from_environment({"OTC_SECRETS_FILE": path})


class EffectiveEnvironmentTests(unittest.TestCase):
    @staticmethod
    def _module():
        spec = importlib.util.spec_from_file_location("otc_process_env", PROCESS_ENV)
        if spec is None or spec.loader is None:
            raise AssertionError("process environment verifier cannot load")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    @staticmethod
    def _expected() -> dict[str, str]:
        return {
            "OTC_ACCEPTING_ORDERS": "1",
            "DB_PATH": "/var/lib/btc09-otc/otc_bot.db",
            "BTC09_WALLET_PATH": "/var/lib/btc09-otc/wallet-mainnet.json",
            "PUBLIC_FEED_PATH": "/var/lib/btc09-otc-public/otc-bot-feed.json",
            "BTC09_BIN": "/opt/btc09/btc09",
            "BTC09_DATADIR": "/opt/btc09/data",
            "BTC09_NETWORK": "btc09-mainnet",
        }

    def test_exact_effective_environment_passes_without_secret_output(self) -> None:
        module = self._module()
        values = self._expected() | {"UNRELATED_SECRET": "do-not-print"}
        blob = (
            b"\x00".join(f"{key}={value}".encode() for key, value in values.items())
            + b"\x00"
        )
        self.assertEqual(module.verify_environment_blob(blob), None)

    def test_mismatch_duplicate_missing_and_oversized_environment_fail(self) -> None:
        module = self._module()
        valid = self._expected()
        cases = []
        mismatch = dict(valid)
        mismatch["OTC_ACCEPTING_ORDERS"] = "0"
        cases.append(mismatch)
        missing = dict(valid)
        del missing["DB_PATH"]
        cases.append(missing)
        for values in cases:
            blob = (
                b"\x00".join(f"{key}={value}".encode() for key, value in values.items())
                + b"\x00"
            )
            with self.assertRaises(module.EnvironmentVerificationError):
                module.verify_environment_blob(blob)
        duplicate = b"OTC_ACCEPTING_ORDERS=1\x00OTC_ACCEPTING_ORDERS=0\x00"
        with self.assertRaises(module.EnvironmentVerificationError):
            module.verify_environment_blob(duplicate)
        with self.assertRaises(module.EnvironmentVerificationError):
            module.verify_environment_blob(b"x" * (1_048_576 + 1))

    def test_dependency_lock_is_exact_and_complete(self) -> None:
        lines = [
            line
            for line in REQUIREMENTS_LOCK.read_text(encoding="ascii").splitlines()
            if line and not line.startswith("#")
        ]
        self.assertEqual(len(lines), 18)
        self.assertTrue(all(line.count("==") == 1 for line in lines))
        self.assertIn("discord.py==2.7.1", lines)
        self.assertIn("cryptography==49.0.0", lines)
        self.assertIn("aiohttp==3.14.1", lines)

    def test_dependency_readback_excludes_only_bootstrap_packages(self) -> None:
        spec = importlib.util.spec_from_file_location("otc_lock_verify", VERIFY_LOCK)
        if spec is None or spec.loader is None:
            raise AssertionError("lock verifier cannot load")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        lock = "# exact\nTyping_Extensions==4.16.0\naiohttp==3.14.1\n"
        module.verify_lock(
            lock,
            {
                "typing-extensions": "4.16.0",
                "aiohttp": "3.14.1",
                "pip": "26.1",
                "setuptools": "82.0",
                "wheel": "0.46",
            },
        )
        for installed in (
            {"typing-extensions": "4.16.0", "aiohttp": "3.14.1", "build": "1"},
            {"typing-extensions": "4.16.0"},
        ):
            with self.assertRaises(module.LockVerificationError):
                module.verify_lock(lock, installed)
        with self.assertRaises(module.LockVerificationError):
            module.verify_lock("aiohttp @ https://example.invalid/archive.whl\n", {})


class MigrationPreflightTests(unittest.TestCase):
    def _run(self, db: Path) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(PREFLIGHT), str(db)],
            cwd=Path(tempfile.gettempdir()),
            capture_output=True,
            text=True,
        )

    def test_exact_live_prototype_zero_aggregate_passes_from_neutral_cwd(self) -> None:
        from bot.otc.store import _LIVE_PROTOTYPE_SOURCE

        with tempfile.TemporaryDirectory() as directory:
            db = Path(directory) / "legacy.db"
            connection = sqlite3.connect(db)
            connection.executescript(_LIVE_PROTOTYPE_SOURCE)
            connection.close()
            result = self._run(db)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                result.stdout.strip(),
                "OTC migration preflight passed (legacy_prototype, zero obligations)",
            )

    def test_exact_incremental_catalog_with_one_compatible_user_passes(self) -> None:
        from bot.otc.store import _LIVE_INCREMENTAL_PROTOTYPE_SOURCE

        with tempfile.TemporaryDirectory() as directory:
            db = Path(directory) / "incremental.db"
            connection = sqlite3.connect(db)
            connection.executescript(_LIVE_INCREMENTAL_PROTOTYPE_SOURCE)
            connection.execute("INSERT INTO users VALUES(7,'Pilot',NULL,1,2)")
            connection.commit()
            connection.close()
            result = self._run(db)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                result.stdout.strip(),
                "OTC migration preflight passed (legacy_prototype, zero obligations)",
            )
            connection = sqlite3.connect(db)
            try:
                self.assertEqual(
                    connection.execute("SELECT * FROM users").fetchall(),
                    [(7, "Pilot", None, 1, 2)],
                )
                self.assertIsNone(
                    connection.execute(
                        "SELECT 1 FROM sqlite_master WHERE name='schema_meta'"
                    ).fetchone()
                )
            finally:
                connection.close()

    def test_incremental_catalog_near_misses_fail_with_sanitized_output(self) -> None:
        from bot.otc.store import _LIVE_INCREMENTAL_PROTOTYPE_SOURCE

        variants = {
            "column_order": _LIVE_INCREMENTAL_PROTOTYPE_SOURCE.replace(
                "ALTER TABLE orders ADD COLUMN deposit_addr TEXT;\n"
                "ALTER TABLE orders ADD COLUMN deposit_expected TEXT;",
                "ALTER TABLE orders ADD COLUMN deposit_expected TEXT;\n"
                "ALTER TABLE orders ADD COLUMN deposit_addr TEXT;",
            ),
            "column_definition": _LIVE_INCREMENTAL_PROTOTYPE_SOURCE.replace(
                "ALTER TABLE orders ADD COLUMN deposit_addr TEXT;",
                "ALTER TABLE orders ADD COLUMN deposit_addr BLOB;",
            ),
            "extra_index": _LIVE_INCREMENTAL_PROTOTYPE_SOURCE
            + "CREATE INDEX private_secret_index ON orders(currency);\n",
            "missing_index": _LIVE_INCREMENTAL_PROTOTYPE_SOURCE.replace(
                "CREATE INDEX idx_orders_buyer ON orders(buyer_id);", ""
            ),
        }
        for name, source in variants.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                db = Path(directory) / "near-miss.db"
                connection = sqlite3.connect(db)
                connection.executescript(source)
                connection.close()
                result = self._run(db)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertEqual(
                    result.stderr.strip(), "OTC migration preflight failed"
                )
                self.assertNotIn("private_secret_index", result.stderr)

    def test_preflight_reuses_store_strict_validator_without_loose_columns(
        self,
    ) -> None:
        text = PREFLIGHT.read_text(encoding="utf-8")
        self.assertIn("Store.validate_migration_preflight", text)
        self.assertNotIn("PRAGMA table_info", text)
        self.assertNotIn("issubset", text)
        self.assertNotIn("def _columns", text)

    def test_legacy_order_and_withdrawal_each_fail_without_row_output(self) -> None:
        from bot.otc.store import _LIVE_PROTOTYPE_SOURCE

        inserts = (
            "INSERT INTO orders(seller_id,amount,price,currency,status,created_at,updated_at) VALUES(1,'1','1','AUD','pending_deposit',1,1)",
            "INSERT INTO withdrawals(admin_id,amount,address,status,created_at) VALUES(1,'1','secret-address','sent',1)",
        )
        for insert in inserts:
            with self.subTest(insert=insert.split(" ", 3)[2]):
                with tempfile.TemporaryDirectory() as directory:
                    db = Path(directory) / "legacy.db"
                    connection = sqlite3.connect(db)
                    connection.executescript(_LIVE_PROTOTYPE_SOURCE)
                    connection.execute(insert)
                    connection.commit()
                    connection.close()
                    result = self._run(db)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("secret-address", result.stdout + result.stderr)

    def test_empty_v4_passes_but_unresolved_transfer_fails(self) -> None:
        from bot.otc.store import Store

        with tempfile.TemporaryDirectory() as directory:
            db = Path(directory) / "v4.db"
            Store(db).initialize()
            self.assertEqual(self._run(db).returncode, 0)
            connection = sqlite3.connect(db)
            try:
                connection.execute("PRAGMA foreign_keys=OFF")
                connection.execute("PRAGMA ignore_check_constraints=ON")
                for (trigger,) in connection.execute(
                    "SELECT name FROM sqlite_schema WHERE type='trigger'"
                ).fetchall():
                    connection.execute(f'DROP TRIGGER "{trigger}"')
                connection.execute(
                    "INSERT INTO transfers(transfer_id,kind,state,amount_units,network_fee_units,destination,created_at,updated_at,operation_key,wallet_scope,attempt_count,earned_fee_units,is_main_outcome) VALUES(1,'fee_withdrawal','queued',1,0,'private',1,1,'test:1','escrow',0,0,0)"
                )
                connection.commit()
            finally:
                connection.close()
            self.assertNotEqual(self._run(db).returncode, 0)


class RestartAdapterTests(unittest.TestCase):
    def test_adapter_uses_real_v4_store_and_trade_service_exact_txid_recovery(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            db = root / "isolated.db"
            wallet = root / "wallet-regtest.json"
            state = root / "state"
            base = [sys.executable, str(ADAPTER)]
            subprocess.run(
                base + ["prepare", "--db", str(db), "--wallet", str(wallet)],
                cwd=ROOT,
                check=True,
            )
            first = subprocess.Popen(
                base
                + [
                    "serve",
                    "--db",
                    str(db),
                    "--wallet",
                    str(wallet),
                    "--state-dir",
                    str(state),
                ],
                cwd=ROOT,
            )
            try:
                for _ in range(100):
                    if (state / "ready.json").exists():
                        ready = json.loads(
                            (state / "ready.json").read_text(encoding="ascii")
                        )
                        if ready.get("generation") == 1:
                            break
                    time.sleep(0.05)
                else:
                    self.fail("first restart-adapter generation did not become ready")
                self.assertFalse((state / "recovery.json").exists())
            finally:
                first.terminate()
                first.wait(timeout=10)
            subprocess.run(
                base + ["inject", "--db", str(db), "--wallet", str(wallet)],
                cwd=ROOT,
                check=True,
            )
            connection = sqlite3.connect(db)
            try:
                self.assertEqual(
                    connection.execute("SELECT version FROM schema_meta").fetchone(),
                    (4,),
                )
                self.assertEqual(
                    connection.execute("SELECT state FROM transfers").fetchone(),
                    ("prepared",),
                )
            finally:
                connection.close()
            process = subprocess.Popen(
                base
                + [
                    "serve",
                    "--db",
                    str(db),
                    "--wallet",
                    str(wallet),
                    "--state-dir",
                    str(state),
                ],
                cwd=ROOT,
            )
            try:
                for _ in range(100):
                    if (state / "ready.json").exists():
                        ready = json.loads(
                            (state / "ready.json").read_text(encoding="ascii")
                        )
                        if ready.get("generation") == 2:
                            break
                    time.sleep(0.05)
                else:
                    self.fail("restart adapter did not become ready")
                subprocess.run(
                    base + ["verify", "--db", str(db), "--state-dir", str(state)],
                    cwd=ROOT,
                    check=True,
                )
                evidence = json.loads(
                    (state / "recovery.json").read_text(encoding="ascii")
                )
                self.assertEqual(
                    evidence["transaction_calls"], [evidence["expected_txid"]]
                )
                self.assertNotEqual(evidence["expected_txid"], evidence["decoy_txid"])
            finally:
                process.terminate()
                process.wait(timeout=10)


class HealthScriptTests(unittest.TestCase):
    def setUp(self) -> None:
        if not GIT_BASH.exists():
            self.skipTest("Git Bash is unavailable")
        self.temp = tempfile.TemporaryDirectory()
        self.db = Path(self.temp.name) / "otc.db"
        _initialized_db(self.db)
        _HealthHandler.payload = _healthy(accepting=False)
        _HealthHandler.status = 503
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _HealthHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def _run(
        self, expected: str = "0", *, url: str | None = None
    ) -> subprocess.CompletedProcess[str]:
        if url is None:
            url = f"http://127.0.0.1:{self.server.server_port}/healthz"
        unix_db = subprocess.run(
            [str(GIT_BASH), "-lc", 'cygpath -u "$1"', "bash", str(self.db)],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        return subprocess.run(
            [str(GIT_BASH), str(HEALTH), url, unix_db, expected],
            cwd=ROOT,
            capture_output=True,
            text=True,
            timeout=10,
        )

    def test_disabled_healthy_service_and_database_pass_without_row_output(
        self,
    ) -> None:
        result = self._run()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            result.stdout.strip(), "OTC health check passed (accepting_orders=0)"
        )

    def test_malformed_http_and_boolean_fail_closed(self) -> None:
        _HealthHandler.payload = {"accepting_orders": 0}
        self.assertNotEqual(self._run().returncode, 0)

    def test_noncanonical_loopback_url_is_rejected(self) -> None:
        result = self._run(
            url="http://127.0.0.1:1@evil.invalid/healthz",
        )
        self.assertNotEqual(result.returncode, 0)
        _HealthHandler.payload = _healthy(accepting=False)
        _HealthHandler.payload["accepting_orders"] = 0
        self.assertNotEqual(self._run().returncode, 0)

    def test_unresolved_address_transfer_and_schema_states_fail(self) -> None:
        for mutation in ("address", "transfer", "schema"):
            with self.subTest(mutation=mutation):
                connection = sqlite3.connect(self.db)
                try:
                    if mutation == "address":
                        connection.execute(
                            "INSERT INTO orders VALUES(1,'address_pending')"
                        )
                    elif mutation == "transfer":
                        connection.execute("INSERT INTO transfers VALUES(1,'prepared')")
                    else:
                        connection.execute("UPDATE schema_meta SET version=3")
                    connection.commit()
                finally:
                    connection.close()
                self.assertNotEqual(self._run().returncode, 0)
                self.db.unlink()
                _initialized_db(self.db)

    def test_foreign_key_and_integrity_failures_are_rejected(self) -> None:
        payload = _healthy(accepting=False)
        payload["foreign_key_integrity"] = "failed"
        _HealthHandler.payload = payload
        self.assertNotEqual(self._run().returncode, 0)
        payload = _healthy(accepting=False)
        payload["integrity"] = "failed"
        _HealthHandler.payload = payload
        self.assertNotEqual(self._run().returncode, 0)


if __name__ == "__main__":
    unittest.main()
