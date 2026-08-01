#!/usr/bin/env python3
"""Bitcoin 09 Discord OTC composition root."""

from __future__ import annotations

import asyncio
import concurrent.futures
import os
import posixpath
import stat
import sys
import time
from collections.abc import Mapping
from dataclasses import dataclass, field
from pathlib import Path

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import discord
from discord.ext import commands

from bot.otc.discord_ui import (
    DiscordTradeUI,
    register_persistent_order_views,
    register_trade_ui,
)
from bot.otc.domain import (
    DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER,
    DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_TOTAL,
    DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER,
    DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
    DEFAULT_MAX_ACTIVE_ORDERS_PER_MAKER,
    DEFAULT_MAX_ACTIVE_ORDERS_TOTAL,
    MAX_CONFIGURABLE_ACTIVE_ORDERS_TOTAL,
    MAX_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
    parse_09c,
)
from bot.otc.explorer import AddressBatch, Explorer, Tip, TransactionStatus
from bot.otc.public_feed import (
    DEFAULT_PUBLIC_FEED_PATH,
    build_public_feed,
    invalidate_public_feed,
    write_public_feed,
)
from bot.otc.service import TradeService
from bot.otc.store import Store
from bot.otc.wallet import Wallet
from bot.otc.translation import translation_provider_from_environment

BOT_VERSION = "otc-trade-v1"
MAX_OTC_SECRETS_BYTES = 16_384
MAX_OTC_SECRET_LINES = 32
MAX_OTC_SECRET_VALUE_BYTES = 4_096
OTC_SECRET_KEYS = frozenset(
    {
        "BOT_TOKEN",
        "DISCORD_BOT_TOKEN",
        "DISCORD_GUILD_ID",
        "ADMIN_IDS",
        "TRANSLATION_API_URL",
        "TRANSLATION_API_TOKEN",
        "OTC_ADMIN_FEE_DESTINATION",
    }
)


def _publish_feed(store: Store, path: str, checked_at: int, runtime_health) -> None:
    payload = build_public_feed(
        store, generated_at=checked_at, runtime_health=runtime_health
    )
    write_public_feed(path, payload)


def _probe_explorer_anchors(explorer: object, read_watched_addresses) -> dict[str, object]:
    issues: list[str] = []
    tip: Tip | None = None
    try:
        candidate = explorer.tip()
        if isinstance(candidate, Tip):
            tip = candidate
        else:
            issues.append("explorer_tip_invalid")
    except Exception:
        issues.append("explorer_tip_failure")

    snapshot_reachable = False
    try:
        batch = explorer.batch_outputs(read_watched_addresses)
        if not isinstance(batch, AddressBatch):
            issues.append("explorer_snapshot_invalid")
        elif tip is None or batch.tip != tip:
            issues.append("explorer_snapshot_tip_mismatch")
        elif batch.network != getattr(explorer, "network", None) or any(
            snapshot.tip != tip or snapshot.network != batch.network
            for snapshot in batch.snapshots
        ):
            issues.append("explorer_snapshot_anchor_mismatch")
        else:
            snapshot_reachable = True
    except Exception:
        issues.append("explorer_snapshot_failure")

    transaction_reachable = False
    try:
        txid = "0" * 64
        status = explorer.transaction(txid)
        if not isinstance(status, TransactionStatus) or status.txid != txid:
            issues.append("explorer_transaction_invalid")
        elif tip is None or status.tip != tip:
            issues.append("explorer_transaction_tip_mismatch")
        else:
            transaction_reachable = True
    except Exception:
        issues.append("explorer_transaction_failure")
    return {
        "explorer_snapshot_reachable": snapshot_reachable,
        "explorer_tx_status_reachable": transaction_reachable,
        "explorer_tip": (
            None if tip is None else {"hash": tip.hash, "height": tip.height}
        ),
        "issues": tuple(sorted(set(issues))),
    }


def _is_private_systemd_credential_copy(
    path: str,
    credential_directory: str | None,
    path_entry: os.stat_result,
) -> bool:
    if (
        os.name == "nt"
        or credential_directory is None
        or posixpath.dirname(credential_directory) != "/run/credentials"
        or posixpath.dirname(path) != credential_directory
        or not posixpath.basename(path)
        or stat.S_IMODE(path_entry.st_mode) != 0o440
        or path_entry.st_uid != 0
        or path_entry.st_gid != 0
    ):
        return False
    try:
        directory_entry = os.lstat(credential_directory)
        parent_entry = os.lstat("/run/credentials")
    except OSError:
        return False
    return (
        stat.S_ISDIR(directory_entry.st_mode)
        and directory_entry.st_nlink == 2
        and stat.S_IMODE(directory_entry.st_mode) == 0o550
        and directory_entry.st_uid == 0
        and directory_entry.st_gid == 0
        and directory_entry.st_dev == path_entry.st_dev
        and stat.S_ISDIR(parent_entry.st_mode)
        and stat.S_IMODE(parent_entry.st_mode) == 0o755
        and parent_entry.st_uid == 0
        and parent_entry.st_gid == 0
        and directory_entry.st_dev != parent_entry.st_dev
    )


def load_otc_secrets(
    path: str, *, credential_directory: str | None = None
) -> dict[str, str]:
    path_module = os.path if os.name == "nt" else posixpath
    if (
        type(path) is not str
        or not path
        or not path_module.isabs(path)
        or "\x00" in path
        or path_module.normpath(path) != path
    ):
        raise ValueError("OTC credential path is invalid")
    if credential_directory is not None and (
        type(credential_directory) is not str
        or not credential_directory
        or not path_module.isabs(credential_directory)
        or "\x00" in credential_directory
        or path_module.normpath(credential_directory) != credential_directory
    ):
        raise ValueError("OTC credential directory is invalid")
    try:
        path_entry = os.lstat(path)
    except OSError:
        raise ValueError("OTC credential file is unavailable") from None
    if not stat.S_ISREG(path_entry.st_mode) or path_entry.st_nlink != 1:
        raise ValueError("OTC credential file is unsafe")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError:
        raise ValueError("OTC credential file is unavailable") from None
    try:
        before = os.fstat(descriptor)
        same_path_entry = (
            path_entry.st_dev,
            path_entry.st_ino,
            path_entry.st_mode,
            path_entry.st_nlink,
            path_entry.st_size,
            path_entry.st_mtime_ns,
        ) == (
            before.st_dev,
            before.st_ino,
            before.st_mode,
            before.st_nlink,
            before.st_size,
            before.st_mtime_ns,
        )
        owner_only = stat.S_IMODE(before.st_mode) & 0o077 == 0
        systemd_credential_copy = _is_private_systemd_credential_copy(
            path, credential_directory, before
        )
        if (
            not same_path_entry
            or not stat.S_ISREG(before.st_mode)
            or before.st_nlink != 1
            or (os.name != "nt" and not (owner_only or systemd_credential_copy))
        ):
            raise ValueError("OTC credential file is unsafe")
        encoded = os.read(descriptor, MAX_OTC_SECRETS_BYTES + 1)
        after = os.fstat(descriptor)
        if (
            len(encoded) > MAX_OTC_SECRETS_BYTES
            or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
            != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
            or len(encoded) != after.st_size
        ):
            raise ValueError("OTC credential file is oversized or changed while reading")
    finally:
        os.close(descriptor)
    try:
        text = encoded.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise ValueError("OTC credential file is not strict UTF-8") from None
    if "\r" in text or "\x00" in text or text.startswith("\ufeff"):
        raise ValueError("OTC credential file contains invalid characters")
    lines = text.split("\n")
    if lines and lines[-1] == "":
        lines.pop()
    if not lines or len(lines) > MAX_OTC_SECRET_LINES or any(not line for line in lines):
        raise ValueError("OTC credential file has an invalid line count")
    result: dict[str, str] = {}
    for line in lines:
        if "=" not in line:
            raise ValueError("OTC credential line is malformed")
        key, value = line.split("=", 1)
        if key not in OTC_SECRET_KEYS:
            raise ValueError("OTC credential key is not allowlisted")
        if key in result:
            raise ValueError("OTC credential key is duplicated")
        if (
            not value
            or value != value.strip()
            or len(value.encode("utf-8")) > MAX_OTC_SECRET_VALUE_BYTES
            or any(ord(character) < 32 or ord(character) == 127 for character in value)
        ):
            raise ValueError("OTC credential value is invalid")
        result[key] = value
    if "BOT_TOKEN" in result and "DISCORD_BOT_TOKEN" in result:
        raise ValueError("OTC credential token aliases are ambiguous")
    return result


def _enabled(
    name: str, default: str = "0", environment: Mapping[str, str] | None = None
) -> bool:
    values = os.environ if environment is None else environment
    return values.get(name, default).strip().lower() in {"1", "true", "yes", "on"}


def _positive_int(
    name: str, default: int, environment: Mapping[str, str] | None = None
) -> int:
    values = os.environ if environment is None else environment
    raw = values.get(name, str(default)).strip()
    value = int(raw)
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def _bounded_positive_int(
    name: str,
    default: int,
    maximum: int,
    environment: Mapping[str, str] | None = None,
) -> int:
    value = _positive_int(name, default, environment)
    if value > maximum:
        raise ValueError(f"{name} must not exceed {maximum}")
    return value


def _nonnegative_units(
    name: str, default: str, environment: Mapping[str, str] | None = None
) -> int:
    values = os.environ if environment is None else environment
    raw = values.get(name, default).strip()
    if raw == "0":
        return 0
    return parse_09c(raw)


def _admin_ids(raw: str) -> frozenset[int]:
    values: set[int] = set()
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        value = int(part)
        if value <= 0:
            raise ValueError("ADMIN_IDS contains a non-positive Discord ID")
        values.add(value)
    return frozenset(values)


@dataclass(frozen=True, slots=True)
class Config:
    token: str = field(repr=False)
    guild_id: int
    admin_ids: frozenset[int]
    accepting_orders_requested: bool
    db_path: str
    explorer_url: str
    network: str
    binary_path: str
    data_dir: str
    wallet_path: str
    seeds: str
    confirmation_depth: int
    network_fee_units: int
    fee_bps: int
    deposit_timeout_seconds: int
    trade_timeout_seconds: int
    reconciliation_interval_seconds: int
    admin_fee_destination: str | None = field(repr=False)
    translation_api_url: str = field(repr=False)
    translation_api_token: str = field(repr=False)
    fee_withdrawal_network_fee_units: int
    public_feed_path: str
    max_active_orders_total: int
    max_active_orders_per_maker: int
    max_deposit_allocations_lifetime_total: int
    max_deposit_allocations_lifetime_per_seller: int
    max_deposit_allocations_daily_total: int
    max_deposit_allocations_daily_per_seller: int

    @classmethod
    def from_environment(
        cls, environment: Mapping[str, str] | None = None
    ) -> "Config":
        values = os.environ if environment is None else environment
        secrets_path = values.get("OTC_SECRETS_FILE", "").strip()
        credential_directory = values.get("CREDENTIALS_DIRECTORY", "").strip()
        secrets = (
            load_otc_secrets(
                secrets_path,
                credential_directory=credential_directory or None,
            )
            if secrets_path
            else {}
        )
        token = (secrets.get("BOT_TOKEN") or secrets.get("DISCORD_BOT_TOKEN") or "").strip()
        guild_id = int(secrets.get("DISCORD_GUILD_ID", "0") or "0")
        if guild_id < 0:
            raise ValueError("DISCORD_GUILD_ID must not be negative")
        network = values.get("BTC09_NETWORK", "btc09-mainnet").strip()
        if network not in {"btc09-mainnet", "btc09-regtest"}:
            raise ValueError("BTC09_NETWORK must be btc09-mainnet or btc09-regtest")
        fee_bps = int(values.get("OTC_FEE_BPS", "0"))
        if not 0 <= fee_bps <= 10_000:
            raise ValueError("OTC_FEE_BPS is out of range")
        admin_destination = secrets.get("OTC_ADMIN_FEE_DESTINATION", "").strip() or None
        max_active_orders_total = _bounded_positive_int(
            "OTC_MAX_ACTIVE_ORDERS_TOTAL",
            DEFAULT_MAX_ACTIVE_ORDERS_TOTAL,
            MAX_CONFIGURABLE_ACTIVE_ORDERS_TOTAL,
            values,
        )
        max_active_orders_per_maker = _bounded_positive_int(
            "OTC_MAX_ACTIVE_ORDERS_PER_MAKER",
            DEFAULT_MAX_ACTIVE_ORDERS_PER_MAKER,
            max_active_orders_total,
            values,
        )
        allocation_lifetime_total = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL",
            DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
            MAX_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
            values,
        )
        allocation_lifetime_seller = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER",
            DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER,
            allocation_lifetime_total,
            values,
        )
        allocation_daily_total = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_DAILY_TOTAL",
            DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_TOTAL,
            allocation_lifetime_total,
            values,
        )
        allocation_daily_seller = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER",
            DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER,
            min(allocation_lifetime_seller, allocation_daily_total),
            values,
        )
        return cls(
            token=token,
            guild_id=guild_id,
            admin_ids=_admin_ids(secrets.get("ADMIN_IDS", "")),
            accepting_orders_requested=_enabled("OTC_ACCEPTING_ORDERS", "0", values),
            db_path=values.get("DB_PATH", "/var/lib/btc09-otc/otc_bot.db"),
            explorer_url=values.get("EXPLORER_URL", "http://127.0.0.1:8009").rstrip("/"),
            network=network,
            binary_path=values.get("BTC09_BIN", "/opt/btc09/btc09"),
            data_dir=values.get("BTC09_DATADIR", "/opt/btc09/data"),
            wallet_path=values.get("BTC09_WALLET_PATH", "/var/lib/btc09-otc/wallet-mainnet.json"),
            seeds=values.get("BTC09_SEEDS", "127.0.0.1:9009").strip(),
            confirmation_depth=_positive_int("OTC_CONFIRMATION_DEPTH", 6, values),
            network_fee_units=_nonnegative_units("BTC09_TX_FEE", "0.0001", values),
            fee_bps=fee_bps,
            deposit_timeout_seconds=_positive_int("ORDER_TIMEOUT_SECONDS", 86_400, values),
            trade_timeout_seconds=_positive_int("TRADE_TIMEOUT_SECONDS", 86_400, values),
            reconciliation_interval_seconds=_positive_int("OTC_RECONCILE_INTERVAL", 30, values),
            admin_fee_destination=admin_destination,
            translation_api_url=secrets.get("TRANSLATION_API_URL", ""),
            translation_api_token=secrets.get("TRANSLATION_API_TOKEN", ""),
            fee_withdrawal_network_fee_units=_nonnegative_units("OTC_FEE_WITHDRAWAL_NETWORK_FEE", "0.0001", values),
            public_feed_path=values.get(
                "PUBLIC_FEED_PATH", str(DEFAULT_PUBLIC_FEED_PATH)
            ),
            max_active_orders_total=max_active_orders_total,
            max_active_orders_per_maker=max_active_orders_per_maker,
            max_deposit_allocations_lifetime_total=allocation_lifetime_total,
            max_deposit_allocations_lifetime_per_seller=allocation_lifetime_seller,
            max_deposit_allocations_daily_total=allocation_daily_total,
            max_deposit_allocations_daily_per_seller=allocation_daily_seller,
        )


@dataclass(frozen=True, slots=True)
class Runtime:
    service: TradeService
    controller: DiscordTradeUI
    public_feed_path: str


def build_runtime(config: Config) -> Runtime:
    Path(config.db_path).parent.mkdir(parents=True, exist_ok=True)
    store = Store(config.db_path, network=config.network)
    store.initialize()
    explorer = Explorer(config.explorer_url, network=config.network)
    wallet = Wallet(
        binary_path=config.binary_path,
        wallet_path=config.wallet_path,
        data_dir=config.data_dir,
        network=config.network,
        seeds=config.seeds,
        explorer=explorer,
    )
    service = TradeService(
        store=store,
        explorer=explorer,
        wallet=wallet,
        fresh_address=wallet.new_address,
        confirmation_depth=config.confirmation_depth,
        clock=lambda: int(time.time()),
        network_fee_units=config.network_fee_units,
        fee_bps=config.fee_bps,
        deposit_timeout_seconds=config.deposit_timeout_seconds,
        trade_timeout_seconds=config.trade_timeout_seconds,
        max_active_orders_total=config.max_active_orders_total,
        max_active_orders_per_maker=config.max_active_orders_per_maker,
        max_deposit_allocations_lifetime_total=config.max_deposit_allocations_lifetime_total,
        max_deposit_allocations_lifetime_per_seller=config.max_deposit_allocations_lifetime_per_seller,
        max_deposit_allocations_daily_total=config.max_deposit_allocations_daily_total,
        max_deposit_allocations_daily_per_seller=config.max_deposit_allocations_daily_per_seller,
    )
    controller = DiscordTradeUI(
        service,
        admin_ids=config.admin_ids,
        accepting_orders=False,
        admin_fee_destination=config.admin_fee_destination,
        fee_withdrawal_network_fee_units=config.fee_withdrawal_network_fee_units,
        translation_provider=translation_provider_from_environment(
            {
                "TRANSLATION_API_URL": config.translation_api_url,
                "TRANSLATION_API_TOKEN": config.translation_api_token,
            }
        ),
    )
    return Runtime(service, controller, config.public_feed_path)


def register_node_owned_commands(bot: commands.Bot) -> None:
    """Keep Node-handled commands present when discord.py bulk-syncs the guild."""

    @bot.tree.command(
        name="stats", description="Show live Bitcoin 09 mining and network stats."
    )
    async def stats_placeholder(_interaction: discord.Interaction) -> None:
        return None

    @bot.tree.command(
        name="rank", description="Show your Bitcoin 09 community activity level."
    )
    async def rank_placeholder(_interaction: discord.Interaction) -> None:
        return None

    @bot.tree.command(
        name="leaderboard",
        description="Show the Bitcoin 09 community activity leaderboard.",
    )
    async def leaderboard_placeholder(_interaction: discord.Interaction) -> None:
        return None

    @bot.tree.command(
        name="wallet", description="Open the current Bitcoin 09 wallet guide."
    )
    async def wallet_placeholder(_interaction: discord.Interaction) -> None:
        return None

    @bot.tree.command(
        name="mine", description="Open the current Bitcoin 09 mining guide."
    )
    async def mine_placeholder(_interaction: discord.Interaction) -> None:
        return None

    support_group = discord.app_commands.Group(
        name="support",
        description="Claim a supporter role or see the current perks.",
    )

    @support_group.command(
        name="claim",
        description="Claim your role after a BTC09 support payment finishes.",
    )
    @discord.app_commands.describe(
        code="The private claim code shown on the BTC09 support page."
    )
    async def support_claim_placeholder(
        _interaction: discord.Interaction, code: str
    ) -> None:
        return None

    @support_group.command(
        name="perks",
        description="Show the current BTC09 supporter roles and perks.",
    )
    async def support_perks_placeholder(_interaction: discord.Interaction) -> None:
        return None

    bot.tree.add_command(support_group)


class OTCBot(commands.Bot):
    def __init__(self, config: Config, runtime: Runtime) -> None:
        intents = discord.Intents.default()
        super().__init__(
            command_prefix="!",
            intents=intents,
            allowed_mentions=discord.AllowedMentions.none(),
        )
        self.config = config
        self.runtime = runtime
        self._reconciler: asyncio.Task[None] | None = None
        self._feed_executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=1, thread_name_prefix="btc09-feed"
        )
        self._closing = False
        register_trade_ui(
            self,
            runtime.controller,
            accepting_orders_requested=config.accepting_orders_requested,
        )
        register_node_owned_commands(self)

    async def setup_hook(self) -> None:
        try:
            await asyncio.to_thread(self.runtime.service.reconcile_transfers)
            await self._refresh_health()
        except Exception as exc:
            self.runtime.controller.accepting_orders = False
            try:
                await self._run_feed_job(
                    invalidate_public_feed, self.runtime.public_feed_path
                )
            except Exception:
                print("[ERROR] OTC feed invalidation failed safely", flush=True)
            print(
                f"[ERROR] OTC startup recovery failed safely: {type(exc).__name__}",
                flush=True,
            )
        await register_persistent_order_views(self, self.runtime.controller)
        self._reconciler = asyncio.create_task(
            self._reconciliation_loop(), name="btc09-otc-reconciliation"
        )
        if self.config.guild_id:
            guild = discord.Object(id=self.config.guild_id)
            self.tree.copy_global_to(guild=guild)
            await self.tree.sync(guild=guild)
            self.tree.clear_commands(guild=None)
            await self.tree.sync()
        else:
            await self.tree.sync()

    async def close(self) -> None:
        self._closing = True
        self.runtime.controller.accepting_orders = False
        if self._reconciler is not None:
            self._reconciler.cancel()
            await asyncio.gather(self._reconciler, return_exceptions=True)
        self._feed_executor.shutdown(wait=True, cancel_futures=True)
        await self.runtime.controller.close_translation()
        try:
            invalidate_public_feed(self.runtime.public_feed_path)
        finally:
            await super().close()

    async def _run_feed_job(self, callable_, *args):
        if self._closing:
            raise RuntimeError("feed work is closing")
        loop = asyncio.get_running_loop()
        return await loop.run_in_executor(self._feed_executor, callable_, *args)

    async def on_ready(self) -> None:
        print(
            f"[INFO] Bitcoin 09 OTC {BOT_VERSION} ready as {self.user}; "
            f"accepting_orders={self.runtime.controller.accepting_orders}",
            flush=True,
        )

    async def _reconciliation_loop(self) -> None:
        while not self.is_closed():
            try:
                await asyncio.to_thread(self.runtime.service.expire_orders)
                await asyncio.to_thread(self.runtime.service.reconcile_transfers)
                await self._refresh_health()
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                self.runtime.controller.accepting_orders = False
                try:
                    await self._run_feed_job(
                        invalidate_public_feed, self.runtime.public_feed_path
                    )
                except Exception:
                    print("[ERROR] OTC feed invalidation failed safely", flush=True)
                print(
                    f"[ERROR] OTC reconciliation failed safely: {type(exc).__name__}",
                    flush=True,
                )
            await asyncio.sleep(self.config.reconciliation_interval_seconds)

    async def _refresh_health(self) -> None:
        if self._closing:
            return
        checked_at = int(time.time())
        service_issues: tuple[str, ...] = ("service_health_unavailable",)
        wallet_spendable_units: int | None = None
        service_accepting = False
        try:
            health = await asyncio.to_thread(self.runtime.service.system_health)
        except Exception:
            health = None
        if health is not None:
            checked_at = health.checked_at
            service_issues = health.issues
            wallet_spendable_units = health.wallet_spendable_units
            service_accepting = health.accepting_orders

        explorer_evidence = await asyncio.to_thread(
            _probe_explorer_anchors,
            self.runtime.service.explorer,
            self.runtime.service.store.watched_deposit_addresses,
        )
        explorer_snapshot_reachable = explorer_evidence[
            "explorer_snapshot_reachable"
        ] is True
        explorer_tx_status_reachable = explorer_evidence[
            "explorer_tx_status_reachable"
        ] is True
        explorer_tip = explorer_evidence["explorer_tip"]
        probe_issues = explorer_evidence["issues"]
        if isinstance(probe_issues, tuple):
            service_issues = tuple(sorted(set(service_issues) | set(probe_issues)))
        self.runtime.controller.accepting_orders = (
            self.config.accepting_orders_requested
            and service_accepting
            and explorer_snapshot_reachable
            and explorer_tx_status_reachable
        )
        runtime_health = {
            "accepting_orders": self.runtime.controller.accepting_orders,
            "issues": service_issues,
            "wallet_spendable_units": wallet_spendable_units,
            "explorer_snapshot_reachable": explorer_snapshot_reachable,
            "explorer_tx_status_reachable": explorer_tx_status_reachable,
            "explorer_tip": explorer_tip,
            "checked_at": checked_at,
        }
        try:
            await self._run_feed_job(
                _publish_feed,
                self.runtime.service.store,
                self.runtime.public_feed_path,
                checked_at,
                runtime_health,
            )
        except Exception:
            self.runtime.controller.accepting_orders = False
            await self._run_feed_job(
                invalidate_public_feed, self.runtime.public_feed_path
            )
            raise


def main() -> None:
    config = Config.from_environment()
    if not config.token:
        raise SystemExit("BOT_TOKEN or DISCORD_BOT_TOKEN is required")
    runtime = build_runtime(config)
    OTCBot(config, runtime).run(config.token, log_handler=None)


if __name__ == "__main__":
    main()
