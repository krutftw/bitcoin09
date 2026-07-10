#!/usr/bin/env python3
"""Bitcoin 09 Discord OTC composition root."""

from __future__ import annotations

import asyncio
import concurrent.futures
import os
import sys
import time
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


def _enabled(name: str, default: str = "0") -> bool:
    return os.environ.get(name, default).strip().lower() in {"1", "true", "yes", "on"}


def _positive_int(name: str, default: int) -> int:
    raw = os.environ.get(name, str(default)).strip()
    value = int(raw)
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def _bounded_positive_int(name: str, default: int, maximum: int) -> int:
    value = _positive_int(name, default)
    if value > maximum:
        raise ValueError(f"{name} must not exceed {maximum}")
    return value


def _nonnegative_units(name: str, default: str) -> int:
    raw = os.environ.get(name, default).strip()
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
    admin_fee_destination: str | None
    fee_withdrawal_network_fee_units: int
    public_feed_path: str
    max_active_orders_total: int
    max_active_orders_per_maker: int
    max_deposit_allocations_lifetime_total: int
    max_deposit_allocations_lifetime_per_seller: int
    max_deposit_allocations_daily_total: int
    max_deposit_allocations_daily_per_seller: int

    @classmethod
    def from_environment(cls) -> "Config":
        token = (os.environ.get("BOT_TOKEN") or os.environ.get("DISCORD_BOT_TOKEN") or "").strip()
        guild_id = int(os.environ.get("DISCORD_GUILD_ID", "0") or "0")
        if guild_id < 0:
            raise ValueError("DISCORD_GUILD_ID must not be negative")
        network = os.environ.get("BTC09_NETWORK", "btc09-mainnet").strip()
        if network not in {"btc09-mainnet", "btc09-regtest"}:
            raise ValueError("BTC09_NETWORK must be btc09-mainnet or btc09-regtest")
        fee_bps = int(os.environ.get("OTC_FEE_BPS", "0"))
        if not 0 <= fee_bps <= 10_000:
            raise ValueError("OTC_FEE_BPS is out of range")
        admin_destination = os.environ.get("OTC_ADMIN_FEE_DESTINATION", "").strip() or None
        max_active_orders_total = _bounded_positive_int(
            "OTC_MAX_ACTIVE_ORDERS_TOTAL",
            DEFAULT_MAX_ACTIVE_ORDERS_TOTAL,
            MAX_CONFIGURABLE_ACTIVE_ORDERS_TOTAL,
        )
        max_active_orders_per_maker = _bounded_positive_int(
            "OTC_MAX_ACTIVE_ORDERS_PER_MAKER",
            DEFAULT_MAX_ACTIVE_ORDERS_PER_MAKER,
            max_active_orders_total,
        )
        allocation_lifetime_total = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL",
            DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
            MAX_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL,
        )
        allocation_lifetime_seller = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER",
            DEFAULT_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER,
            allocation_lifetime_total,
        )
        allocation_daily_total = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_DAILY_TOTAL",
            DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_TOTAL,
            allocation_lifetime_total,
        )
        allocation_daily_seller = _bounded_positive_int(
            "OTC_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER",
            DEFAULT_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER,
            min(allocation_lifetime_seller, allocation_daily_total),
        )
        return cls(
            token=token,
            guild_id=guild_id,
            admin_ids=_admin_ids(os.environ.get("ADMIN_IDS", "")),
            accepting_orders_requested=_enabled("OTC_ACCEPTING_ORDERS", "0"),
            db_path=os.environ.get("DB_PATH", "/var/lib/btc09-otc/otc_bot.db"),
            explorer_url=os.environ.get("EXPLORER_URL", "http://127.0.0.1:8009").rstrip("/"),
            network=network,
            binary_path=os.environ.get("BTC09_BIN", "/opt/btc09/btc09"),
            data_dir=os.environ.get("BTC09_DATADIR", "/opt/btc09/data"),
            wallet_path=os.environ.get("BTC09_WALLET_PATH", "/var/lib/btc09-otc/wallet-mainnet.json"),
            seeds=os.environ.get("BTC09_SEEDS", "127.0.0.1:9009").strip(),
            confirmation_depth=_positive_int("OTC_CONFIRMATION_DEPTH", 6),
            network_fee_units=_nonnegative_units("BTC09_TX_FEE", "0.0001"),
            fee_bps=fee_bps,
            deposit_timeout_seconds=_positive_int("ORDER_TIMEOUT_SECONDS", 86_400),
            trade_timeout_seconds=_positive_int("TRADE_TIMEOUT_SECONDS", 86_400),
            reconciliation_interval_seconds=_positive_int("OTC_RECONCILE_INTERVAL", 30),
            admin_fee_destination=admin_destination,
            fee_withdrawal_network_fee_units=_nonnegative_units("OTC_FEE_WITHDRAWAL_NETWORK_FEE", "0.0001"),
            public_feed_path=os.environ.get(
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
        translation_provider=translation_provider_from_environment(),
    )
    return Runtime(service, controller, config.public_feed_path)


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
        register_trade_ui(self, runtime.controller)

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
        else:
            await self.tree.sync()

    async def close(self) -> None:
        self._closing = True
        self.runtime.controller.accepting_orders = False
        if self._reconciler is not None:
            self._reconciler.cancel()
            await asyncio.gather(self._reconciler, return_exceptions=True)
        self._feed_executor.shutdown(wait=True, cancel_futures=True)
        self.runtime.controller.translation_executor.shutdown()
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
