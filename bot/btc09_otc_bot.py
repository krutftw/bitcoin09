#!/usr/bin/env python3
"""Bitcoin 09 Discord OTC composition root."""

from __future__ import annotations

import asyncio
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
from bot.otc.domain import parse_09c
from bot.otc.explorer import Explorer
from bot.otc.service import TradeService
from bot.otc.store import Store
from bot.otc.wallet import Wallet

BOT_VERSION = "otc-trade-v1"


def _enabled(name: str, default: str = "0") -> bool:
    return os.environ.get(name, default).strip().lower() in {"1", "true", "yes", "on"}


def _positive_int(name: str, default: int) -> int:
    raw = os.environ.get(name, str(default)).strip()
    value = int(raw)
    if value <= 0:
        raise ValueError(f"{name} must be positive")
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
        )


@dataclass(frozen=True, slots=True)
class Runtime:
    service: TradeService
    controller: DiscordTradeUI


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
    )
    controller = DiscordTradeUI(
        service,
        admin_ids=config.admin_ids,
        accepting_orders=False,
        admin_fee_destination=config.admin_fee_destination,
        fee_withdrawal_network_fee_units=config.fee_withdrawal_network_fee_units,
    )
    return Runtime(service, controller)


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
        register_trade_ui(self, runtime.controller)

    async def setup_hook(self) -> None:
        try:
            await asyncio.to_thread(self.runtime.service.reconcile_transfers)
            await self._refresh_health()
        except Exception as exc:
            self.runtime.controller.accepting_orders = False
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
        if self._reconciler is not None:
            self._reconciler.cancel()
            await asyncio.gather(self._reconciler, return_exceptions=True)
        await super().close()

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
                print(
                    f"[ERROR] OTC reconciliation failed safely: {type(exc).__name__}",
                    flush=True,
                )
            await asyncio.sleep(self.config.reconciliation_interval_seconds)

    async def _refresh_health(self) -> None:
        health = await asyncio.to_thread(self.runtime.service.system_health)
        self.runtime.controller.accepting_orders = (
            self.config.accepting_orders_requested and health.accepting_orders
        )


def main() -> None:
    config = Config.from_environment()
    if not config.token:
        raise SystemExit("BOT_TOKEN or DISCORD_BOT_TOKEN is required")
    runtime = build_runtime(config)
    OTCBot(config, runtime).run(config.token, log_handler=None)


if __name__ == "__main__":
    main()
