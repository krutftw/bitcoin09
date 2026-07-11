from __future__ import annotations

import asyncio
import re
from collections import OrderedDict
from collections.abc import Awaitable, Callable, Iterable
from dataclasses import dataclass
from typing import Any

import discord
from discord import app_commands

from bot.otc.domain import (
    PUBLIC_SETTLEMENT_METHODS,
    PUBLIC_SETTLEMENT_NETWORKS,
    parse_09c,
    parse_asset,
)
from bot.otc.service import (
    AccountStatus,
    AuthorizationError,
    OrderConflict,
    OrderResult,
    PublicOrderResult,
    TradeService,
    TradeServiceError,
)
from bot.otc.translation import (
    DisabledTranslationProvider,
    TranslationBusy,
    TranslationExecutor,
    TranslationProvider,
    TranslationUnavailable,
)

COMMON_ASSETS = (
    "AUD",
    "USD",
    "EUR",
    "GBP",
    "CNY",
    "JPY",
    "USDT",
    "USDC",
    "BTC",
    "ETH",
    "SOL",
    "LTC",
    "DOGE",
    "BNB",
)
MAINTENANCE_NOTICE = (
    "New OTC escrow actions are temporarily paused while the safer WTS/WTB "
    "trade system completes its controlled pilot. Do not send 09C to an old "
    "deposit address. Follow #announcements for the verified launch."
)
_CUSTOM_ACTION_RE = re.compile(r"[a-z][a-z0-9_]{0,31}\Z", re.ASCII)
_FUND_ACTIONS = frozenset(
    {
        "confirm_sent",
        "confirm_received",
        "cancel",
        "resolve",
        "reconcile",
        "mine",
        "withdraw",
    }
)


def format_09c(units: int) -> str:
    whole, fraction = divmod(units, 100_000_000)
    return str(whole) if fraction == 0 else f"{whole}.{fraction:08d}".rstrip("0")


def _public_label(value: str | None, *, network: bool) -> str:
    if value is None or not value.strip():
        return "Not specified"
    text = " ".join(value.strip().split())
    allowed = PUBLIC_SETTLEMENT_NETWORKS if network else PUBLIC_SETTLEMENT_METHODS
    if text in allowed:
        return text
    return "Private settlement network" if network else "Private settlement method"


def render_public_order(order: OrderResult | PublicOrderResult) -> str:
    side = "WTS" if order.side == "sell" else "WTB"
    network = _public_label(order.settlement_network, network=True)
    method = _public_label(order.payment_method, network=False)
    return (
        f"Order #{order.order_id} | {side} | {format_09c(order.net_amount_units)} 09C\n"
        f"Total price: {order.total_price} {order.settlement_asset}\n"
        f"Network / method: {network} / {method}\n"
        f"Status: {order.state}"
    )


def render_private_order(order: OrderResult, *, actor_id: int) -> str:
    text = render_public_order(order)
    if actor_id == order.seller_id and order.deposit_addr:
        text += (
            "\nSeller deposit address: "
            f"{order.deposit_addr}\nDeposit required: "
            f"{format_09c(order.deposit_required_units)} 09C"
        )
    return text


def asset_choices(current: str) -> tuple[str, ...]:
    candidate = current.strip().upper()
    matches = [asset for asset in COMMON_ASSETS if candidate in asset]
    if candidate and candidate not in matches:
        try:
            custom = parse_asset(candidate)
        except ValueError:
            pass
        else:
            matches.insert(0, custom)
    return tuple(dict.fromkeys(matches[:25]))


def action_custom_id(action: str, order_id: int) -> str:
    if not _CUSTOM_ACTION_RE.fullmatch(action):
        raise ValueError("component action is invalid")
    if type(order_id) is not int or order_id <= 0:
        raise ValueError("component order ID is invalid")
    return f"{action}:{order_id}"


@dataclass(frozen=True, slots=True)
class _CachedResult:
    value: object
    failed: bool = False


class _ConfirmationView(discord.ui.View):
    def __init__(
        self,
        controller: "DiscordTradeUI",
        *,
        action: str,
        order_id: int,
        callback: Callable[[discord.Interaction], Awaitable[str | None]],
    ) -> None:
        super().__init__(timeout=300)
        self._controller = controller
        self._callback = callback
        self._lock = asyncio.Lock()
        self._consumed = False
        self._result_message: str | None = None
        self._completion: asyncio.Future[str | None] | None = None
        button = discord.ui.Button(
            label="Confirm",
            style=discord.ButtonStyle.danger,
            custom_id=action_custom_id(f"confirmed_{action}", order_id),
        )
        button.callback = self._confirm  # type: ignore[method-assign]
        self.add_item(button)

    async def _confirm(self, interaction: discord.Interaction) -> None:
        owner = False
        async with self._lock:
            if not self._consumed:
                owner = True
                self._consumed = True
                self._completion = asyncio.get_running_loop().create_future()
                for child in self.children:
                    child.disabled = True
            completion = self._completion
        if completion is None:
            raise RuntimeError("confirmation completion state is missing")
        if not owner:
            await self._controller._defer(interaction, ephemeral=True)
            result = await asyncio.shield(completion)
            await self._controller._respond(
                interaction,
                result or "This confirmation was already processed safely.",
                ephemeral=True,
            )
            return
        acknowledgement_failure = (
            "Confirmation could not be acknowledged safely. "
            "No trade action was performed."
        )
        try:
            await interaction.response.edit_message(
                content="Confirmed. Processing this action safely...", view=self
            )
        except BaseException:
            self._result_message = acknowledgement_failure
            if not completion.done():
                completion.set_result(acknowledgement_failure)
            raise
        try:
            self._result_message = await self._callback(interaction)
        except BaseException:
            callback_failure = (
                "The confirmed action did not finish normally. "
                "Check the current order state before trying anything else."
            )
            self._result_message = callback_failure
            if not completion.done():
                completion.set_result(callback_failure)
            raise
        if not completion.done():
            completion.set_result(self._result_message)


class TradeOrderModal(discord.ui.Modal):
    def __init__(self, controller: "DiscordTradeUI", *, side: str) -> None:
        title = "Create WTS offer" if side == "sell" else "Create WTB offer"
        super().__init__(title=title, custom_id=action_custom_id(side, 1))
        self.controller = controller
        self.side = side
        self.amount = discord.ui.TextInput(label="09C amount", max_length=32)
        self.total_price = discord.ui.TextInput(label="Total settlement price", max_length=37)
        self.asset = discord.ui.TextInput(label="Asset code, for example AUD or USDT", max_length=12)
        self.method = discord.ui.TextInput(label="Method, for example PayID or Wise", max_length=32)
        self.network = discord.ui.TextInput(
            label="Network, if applicable", required=False, max_length=48
        )
        for item in (
            self.amount,
            self.total_price,
            self.asset,
            self.method,
            self.network,
        ):
            self.add_item(item)

    async def on_submit(self, interaction: discord.Interaction) -> None:
        method = self.controller.create_sell if self.side == "sell" else self.controller.create_buy
        await method(
            interaction,
            str(self.amount),
            str(self.total_price),
            str(self.asset),
            str(self.method),
            str(self.network) or None,
            None,
        )


class DisputeModal(discord.ui.Modal):
    def __init__(self, controller: "DiscordTradeUI", *, order_id: int) -> None:
        super().__init__(
            title=f"Dispute order #{order_id}",
            custom_id=action_custom_id("dispute", order_id),
        )
        self.controller = controller
        self.order_id = order_id
        self.reason = discord.ui.TextInput(
            label="Private reason", style=discord.TextStyle.paragraph, min_length=10, max_length=500
        )
        self.add_item(self.reason)

    async def on_submit(self, interaction: discord.Interaction) -> None:
        await self.controller.dispute(interaction, self.order_id, str(self.reason))


class AddressModal(discord.ui.Modal):
    def __init__(self, controller: "DiscordTradeUI") -> None:
        super().__init__(title="Set 09C receive address", custom_id=action_custom_id("address", 1))
        self.controller = controller
        self.address = discord.ui.TextInput(label="09C receive/refund address", max_length=128)
        self.add_item(self.address)

    async def on_submit(self, interaction: discord.Interaction) -> None:
        await self.controller.set_address(interaction, str(self.address))


class OrderActionView(discord.ui.View):
    _STATE_ACTIONS = {
        "open": ("accept", "cancel"),
        "awaiting_deposit": ("deposit", "cancel"),
        "matched": ("confirm_sent", "confirm_received", "cancel", "dispute"),
    }

    def __init__(
        self, controller: "DiscordTradeUI", order: OrderResult | PublicOrderResult
    ) -> None:
        super().__init__(timeout=None)
        self.controller = controller
        self.order_id = order.order_id
        for action in self._STATE_ACTIONS.get(order.state, ()):
            style = (
                discord.ButtonStyle.danger
                if action in {"cancel", "dispute"}
                else discord.ButtonStyle.primary
            )
            button = discord.ui.Button(
                label=action.replace("_", " ").title(),
                style=style,
                custom_id=action_custom_id(action, order.order_id),
            )
            button.callback = self._callback(action)  # type: ignore[method-assign]
            self.add_item(button)

    def _callback(
        self, action: str
    ) -> Callable[[discord.Interaction], Awaitable[None]]:
        async def dispatch(interaction: discord.Interaction) -> None:
            if action == "accept":
                await self.controller.accept(interaction, self.order_id)
            elif action == "deposit":
                await self.controller.check_deposit(interaction, self.order_id)
            elif action == "confirm_sent":
                await self.controller.confirm_sent(interaction, self.order_id)
            elif action == "confirm_received":
                await self.controller.confirm_received(interaction, self.order_id)
            elif action == "cancel":
                await self.controller.cancel(interaction, self.order_id)
            elif action == "dispute":
                await interaction.response.send_modal(
                    DisputeModal(self.controller, order_id=self.order_id)
                )

        return dispatch


class DiscordTradeUI:
    """Discord presentation/controller layer that depends only on TradeService."""

    def __init__(
        self,
        service: TradeService,
        *,
        admin_ids: Iterable[int],
        accepting_orders: bool,
        admin_fee_destination: str | None = None,
        fee_withdrawal_network_fee_units: int = 0,
        executor: Callable[[Callable[[], object]], Awaitable[object]] | None = None,
        translation_provider: TranslationProvider | None = None,
        translation_executor: TranslationExecutor | None = None,
    ) -> None:
        self.service = service
        self.admin_ids = frozenset(int(value) for value in admin_ids)
        self.accepting_orders = bool(accepting_orders)
        self.admin_fee_destination = admin_fee_destination
        self.fee_withdrawal_network_fee_units = fee_withdrawal_network_fee_units
        self._executor = executor or asyncio.to_thread
        if translation_executor is not None and translation_provider is not None:
            raise ValueError("configure one translation boundary")
        self.translation_executor = translation_executor or TranslationExecutor(
            translation_provider or DisabledTranslationProvider()
        )
        self._cache: OrderedDict[tuple[int, str], _CachedResult] = OrderedDict()
        self._pending: dict[tuple[int, str], asyncio.Future[object]] = {}
        self._cache_lock = asyncio.Lock()

    async def translate_message(
        self, interaction: discord.Interaction, message: discord.Message
    ) -> None:
        await self._defer(interaction, ephemeral=True)
        source = getattr(message, "content", None)
        if type(source) is not str or not source:
            await self._respond(
                interaction,
                "This message has no text to translate.",
                ephemeral=True,
            )
            return
        try:
            translated = await self.translation_executor.translate_to_english(source)
        except TranslationBusy:
            await self._respond(
                interaction,
                "English translation is busy. Please try again shortly.",
                ephemeral=True,
            )
            return
        except (TranslationUnavailable, ValueError):
            await self._respond(
                interaction,
                "English translation is unavailable right now.",
                ephemeral=True,
            )
            return
        if type(translated) is not str:
            raise RuntimeError("translation provider returned invalid output")
        await self._respond(interaction, translated, ephemeral=True)

    async def close_translation(self) -> None:
        await self.translation_executor.aclose()

    async def create_sell(
        self,
        interaction: Any,
        amount: str,
        total_price: str,
        asset: str,
        method: str,
        network: str | None,
        receive_address: str | None,
    ) -> str | None:
        if not await self._accepting(interaction):
            return None
        await self._defer(interaction, ephemeral=True)
        actor = self._actor_id(interaction)
        return await self._order_call(
            interaction,
            "create_sell",
            lambda: self.service.create_sell(
                seller_id=actor,
                seller_name=self._actor_name(interaction),
                receive_address=receive_address,
                net_amount=parse_09c(amount),
                total_price=total_price,
                asset=parse_asset(asset),
                method=method,
                network=network,
            ),
            private=True,
        )

    async def create_buy(
        self,
        interaction: Any,
        amount: str,
        total_price: str,
        asset: str,
        method: str,
        network: str | None,
        receive_address: str | None,
    ) -> str | None:
        if not await self._accepting(interaction):
            return None
        await self._defer(interaction, ephemeral=True)
        actor = self._actor_id(interaction)
        return await self._order_call(
            interaction,
            "create_buy",
            lambda: self.service.create_buy(
                buyer_id=actor,
                buyer_name=self._actor_name(interaction),
                receive_address=receive_address,
                net_amount=parse_09c(amount),
                total_price=total_price,
                asset=parse_asset(asset),
                method=method,
                network=network,
            ),
            private=True,
        )

    async def list_orders(self, interaction: Any) -> None:
        await self._defer(interaction)
        try:
            method = getattr(self.service, "list_open_public", self.service.list_open)
            orders = await self._run_once(interaction, "list", method)
            values = tuple(orders)  # type: ignore[arg-type]
            content = (
                "No open OTC orders."
                if not values
                else "\n\n".join(render_public_order(item) for item in values)
            )
            await self._followup(interaction, content)
        except Exception as exc:
            await self._error(interaction, exc)

    async def view_order(self, interaction: Any, order_id: int) -> None:
        await self._defer(interaction)
        try:
            order = await self._run_once(
                interaction, f"view:{order_id}", lambda: self.service.view_order(order_id)
            )
            assert isinstance(order, PublicOrderResult)
            action_view = OrderActionView(self, order)
            await self._followup(
                interaction,
                render_public_order(order),
                view=action_view if action_view.children else None,
            )
        except Exception as exc:
            await self._error(interaction, exc)

    async def accept(
        self, interaction: Any, order_id: int, receive_address: str | None = None
    ) -> str | None:
        if not await self._accepting(interaction):
            return None
        await self._defer(interaction, ephemeral=True)
        return await self._order_call(
            interaction,
            f"accept:{order_id}",
            lambda: self.service.accept(
                order_id,
                actor_id=self._actor_id(interaction),
                actor_name=self._actor_name(interaction),
                receive_address=receive_address,
            ),
            private=True,
        )

    async def check_deposit(self, interaction: Any, order_id: int) -> str | None:
        await self._defer(interaction, ephemeral=True)
        return await self._order_call(
            interaction,
            f"deposit:{order_id}",
            lambda: self.service.check_deposit(
                order_id, actor_id=self._actor_id(interaction)
            ),
            private=True,
        )

    async def confirm_sent(
        self, interaction: Any, order_id: int, *, confirmed: bool = False
    ) -> str | None:
        return await self._confirmed_order_action(
            interaction,
            "confirm_sent",
            order_id,
            confirmed,
            lambda active: lambda: self.service.confirm_sent(
                order_id, actor_id=self._actor_id(active)
            ),
        )

    async def confirm_received(
        self, interaction: Any, order_id: int, *, confirmed: bool = False
    ) -> str | None:
        return await self._confirmed_order_action(
            interaction,
            "confirm_received",
            order_id,
            confirmed,
            lambda active: lambda: self.service.confirm_received(
                order_id, actor_id=self._actor_id(active)
            ),
        )

    async def cancel(
        self, interaction: Any, order_id: int, *, confirmed: bool = False
    ) -> str | None:
        return await self._confirmed_order_action(
            interaction,
            "cancel",
            order_id,
            confirmed,
            lambda active: lambda: self.service.cancel(
                order_id, actor_id=self._actor_id(active)
            ),
        )

    async def dispute(self, interaction: Any, order_id: int, reason: str) -> str | None:
        await self._defer(interaction, ephemeral=True)
        return await self._order_call(
            interaction,
            f"dispute:{order_id}",
            lambda: self.service.open_dispute(
                order_id, actor_id=self._actor_id(interaction), reason=reason
            ),
            private=True,
        )

    async def resolve(
        self,
        interaction: Any,
        order_id: int,
        winner: str,
        reason: str,
        *,
        confirmed: bool = False,
    ) -> str | None:
        if not await self._admin(interaction):
            return None
        return await self._confirmed_order_action(
            interaction,
            "resolve",
            order_id,
            confirmed,
            lambda active: lambda: self.service.resolve_dispute(
                order_id,
                admin_id=self._actor_id(active),
                winner=winner,
                reason=reason,
            ),
        )

    async def set_address(self, interaction: Any, address: str) -> None:
        await self._defer(interaction, ephemeral=True)
        try:
            saved = await self._run_once(
                interaction,
                "set_address",
                lambda: self.service.set_receive_address(
                    actor_id=self._actor_id(interaction),
                    actor_name=self._actor_name(interaction),
                    address=address,
                ),
            )
            await self._followup(
                interaction,
                f"Your validated 09C receive/refund address is saved: {saved}",
                ephemeral=True,
            )
        except Exception as exc:
            await self._error(interaction, exc)

    async def balance(self, interaction: Any) -> None:
        await self._defer(interaction, ephemeral=True)
        try:
            status = await self._run_once(
                interaction,
                "account_status",
                lambda: self.service.account_status(actor_id=self._actor_id(interaction)),
            )
            assert isinstance(status, AccountStatus)
            await self._followup(
                interaction,
                "Bitcoin 09 OTC does not hold a custodial user account balance. "
                f"Your orders: {status.order_count} total, "
                f"{status.active_order_count} active, "
                f"{status.completed_order_count} completed, "
                f"{status.disputed_order_count} disputed.",
                ephemeral=True,
            )
        except Exception as exc:
            await self._error(interaction, exc)

    async def reconcile(
        self, interaction: Any, *, confirmed: bool = False
    ) -> str | None:
        if not await self._admin(interaction):
            return None
        if not confirmed:
            await self._confirmation(
                interaction,
                "reconcile",
                1,
                lambda active: self.reconcile(active, confirmed=True),
            )
            return None
        await self._defer(interaction, ephemeral=True)
        try:
            transfers = await self._run_once(
                interaction, "reconcile", self.service.reconcile_transfers
            )
            await self._followup(
                interaction,
                f"Safe reconciliation completed for {len(tuple(transfers))} transfer record(s).",
                ephemeral=True,
            )
            return f"Safe reconciliation completed for {len(tuple(transfers))} transfer record(s)."
        except Exception as exc:
            return await self._error(interaction, exc)

    async def mine(self, interaction: Any, *, confirmed: bool = False) -> str | None:
        if not await self._admin(interaction):
            return None
        if not confirmed:
            await self._confirmation(
                interaction,
                "mine",
                1,
                lambda active: self.mine(active, confirmed=True),
            )
            return None
        await self._defer(interaction, ephemeral=True)
        try:
            result = await self._run_once(interaction, "mine", self.service.mine)
            message = "No wallet operation was ready." if result is None else "One queued wallet operation was processed safely."
            await self._followup(interaction, message, ephemeral=True)
            return message
        except Exception as exc:
            return await self._error(interaction, exc)

    async def withdraw(
        self,
        interaction: Any,
        amount: str,
        destination: str,
        *,
        confirmed: bool = False,
        operation_key: str | None = None,
    ) -> str | None:
        if not await self._admin(interaction):
            return None
        if not confirmed:
            intent_key = (
                operation_key
                or f"discord:{self._interaction_id(interaction)}:fee_withdrawal"
            )
            await self._confirmation(
                interaction,
                "withdraw",
                1,
                lambda confirmed_interaction: self.withdraw(
                    confirmed_interaction,
                    amount,
                    destination,
                    confirmed=True,
                    operation_key=intent_key,
                ),
            )
            return None
        await self._defer(interaction, ephemeral=True)
        if not self.admin_fee_destination:
            await self._followup(
                interaction,
                "Fee withdrawal is disabled because no administrator destination is configured.",
                ephemeral=True,
            )
            return "Fee withdrawal is disabled because no administrator destination is configured."
        try:
            units = parse_09c(amount)
            if operation_key is None:
                raise ValueError("fee withdrawal intent identity is missing")

            def reserve_and_mine() -> tuple[object | None, int]:
                available = self.service.available_fee_units()
                reserved = self.service.reserve_fee_withdrawal(
                    admin_id=self._actor_id(interaction),
                    operation_key=operation_key,
                    amount=units,
                    network_fee=self.fee_withdrawal_network_fee_units,
                    destination=destination,
                    configured_destination=self.admin_fee_destination or "",
                )
                if reserved is not None:
                    self.service.mine()
                return reserved, available

            reserved, available = await self._run_once(
                interaction, "withdraw", reserve_and_mine
            )
            if reserved is None:
                message = (
                    "Fee withdrawal was not reserved. Available confirmed platform "
                    f"fees: {format_09c(available)} 09C."
                )
            else:
                message = "Fee withdrawal was durably reserved for safe processing."
            await self._followup(interaction, message, ephemeral=True)
            return message
        except Exception as exc:
            return await self._error(interaction, exc)

    async def legacy_confirm(
        self, interaction: Any, order_id: int, *, confirmed: bool = False
    ) -> str | None:
        if not confirmed:
            await self._confirmation(
                interaction,
                "confirm",
                order_id,
                lambda active: self.legacy_confirm(active, order_id, confirmed=True),
            )
            return None
        await self._defer(interaction, ephemeral=True)
        actor = self._actor_id(interaction)

        def confirm_role() -> OrderResult:
            try:
                return self.service.confirm_sent(order_id, actor_id=actor)
            except AuthorizationError:
                return self.service.confirm_received(order_id, actor_id=actor)

        return await self._order_call(
            interaction, f"legacy_confirm:{order_id}", confirm_role, private=True
        )

    async def _confirmed_order_action(
        self,
        interaction: Any,
        action: str,
        order_id: int,
        confirmed: bool,
        call_factory: Callable[[Any], Callable[[], OrderResult]],
    ) -> str | None:
        if action not in _FUND_ACTIONS:
            raise ValueError("action does not require confirmation")
        if not confirmed:
            await self._confirmation(
                interaction,
                action,
                order_id,
                lambda confirmed_interaction: self._confirmed_order_action(
                    confirmed_interaction, action, order_id, True, call_factory
                ),
            )
            return None
        await self._defer(interaction, ephemeral=True)
        return await self._order_call(
            interaction,
            f"{action}:{order_id}",
            call_factory(interaction),
            private=True,
        )

    async def _confirmation(
        self,
        interaction: Any,
        action: str,
        order_id: int,
        callback: Callable[[discord.Interaction], Awaitable[str | None]],
    ) -> None:
        await self._defer(interaction, ephemeral=True)
        view = _ConfirmationView(
            self, action=action, order_id=order_id, callback=callback
        )
        await self._followup(
            interaction,
            f"Confirm this {action.replace('_', ' ')} action. It can move or reserve funds.",
            ephemeral=True,
            view=view,
        )

    async def _order_call(
        self,
        interaction: Any,
        key: str,
        call: Callable[[], OrderResult],
        *,
        private: bool,
    ) -> str:
        try:
            result = await self._run_once(interaction, key, call)
            if not isinstance(result, OrderResult):
                raise RuntimeError("trade service returned an invalid order result")
            content = (
                render_private_order(result, actor_id=self._actor_id(interaction))
                if private
                else render_public_order(result)
            )
            action_view = OrderActionView(self, result)
            await self._followup(
                interaction,
                content,
                ephemeral=private,
                view=action_view if action_view.children else None,
            )
            return content
        except Exception as exc:
            return await self._error(interaction, exc)

    async def _run_once(
        self, interaction: Any, action: str, call: Callable[[], object]
    ) -> object:
        key = (self._interaction_id(interaction), action)
        owner = False
        async with self._cache_lock:
            cached = self._cache.get(key)
            if cached is not None:
                self._cache.move_to_end(key)
                if cached.failed:
                    raise cached.value  # type: ignore[misc]
                return cached.value
            future = self._pending.get(key)
            if future is None:
                future = asyncio.get_running_loop().create_future()
                self._pending[key] = future
                owner = True
        if not owner:
            return await asyncio.shield(future)
        try:
            value = await self._executor(call)
        except Exception as exc:
            async with self._cache_lock:
                self._pending.pop(key, None)
                self._cache[key] = _CachedResult(exc, failed=True)
                while len(self._cache) > 2048:
                    self._cache.popitem(last=False)
                if not future.done():
                    future.set_exception(exc)
                    future.exception()
            raise
        async with self._cache_lock:
            self._pending.pop(key, None)
            self._cache[key] = _CachedResult(value)
            while len(self._cache) > 2048:
                self._cache.popitem(last=False)
            if not future.done():
                future.set_result(value)
        return value

    async def _accepting(self, interaction: Any) -> bool:
        if self.accepting_orders:
            return True
        await interaction.response.send_message(MAINTENANCE_NOTICE, ephemeral=True)
        return False

    async def _admin(self, interaction: Any) -> bool:
        if self._actor_id(interaction) in self.admin_ids:
            return True
        if not getattr(interaction.response, "is_done", lambda: False)():
            await interaction.response.send_message(
                "This action is restricted to configured OTC administrators.",
                ephemeral=True,
            )
        else:
            await self._followup(
                interaction,
                "This action is restricted to configured OTC administrators.",
                ephemeral=True,
            )
        return False

    @staticmethod
    async def _defer(interaction: Any, *, ephemeral: bool = False) -> None:
        if not getattr(interaction.response, "is_done", lambda: False)():
            await interaction.response.defer(ephemeral=ephemeral, thinking=True)

    @staticmethod
    async def _followup(
        interaction: Any,
        content: str,
        *,
        ephemeral: bool = False,
        view: discord.ui.View | None = None,
    ) -> None:
        await interaction.followup.send(content, ephemeral=ephemeral, view=view)

    async def _error(self, interaction: Any, exc: Exception) -> str:
        if isinstance(exc, (AuthorizationError, OrderConflict, ValueError)):
            message = str(exc)
        elif isinstance(exc, TradeServiceError):
            message = "Trade action could not be completed safely in its current state."
        else:
            message = "Trade action could not be completed safely. Please try again later."
        await self._followup(interaction, message[:500], ephemeral=True)
        return message[:500]

    async def _respond(
        self, interaction: Any, content: str, *, ephemeral: bool
    ) -> None:
        if not getattr(interaction.response, "is_done", lambda: False)():
            await interaction.response.send_message(content, ephemeral=ephemeral)
        else:
            await self._followup(interaction, content, ephemeral=ephemeral)

    @staticmethod
    def _interaction_id(interaction: Any) -> int:
        value = getattr(interaction, "id", None)
        if type(value) is not int or value <= 0:
            raise ValueError("Discord interaction identity is invalid")
        return value

    @staticmethod
    def _actor_id(interaction: Any) -> int:
        value = getattr(getattr(interaction, "user", None), "id", None)
        if type(value) is not int or value <= 0:
            raise ValueError("Discord user identity is invalid")
        return value

    @staticmethod
    def _actor_name(interaction: Any) -> str:
        user = getattr(interaction, "user", None)
        value = getattr(user, "display_name", None) or getattr(user, "name", None)
        return str(value or "Discord trader")[:128]


async def _asset_autocomplete(
    _interaction: discord.Interaction, current: str
) -> list[app_commands.Choice[str]]:
    return [app_commands.Choice(name=value, value=value) for value in asset_choices(current)]


async def register_persistent_order_views(
    bot: Any, controller: DiscordTradeUI
) -> int:
    orders = await controller._executor(controller.service.list_actionable_public)
    values = tuple(orders)  # type: ignore[arg-type]
    for order in values:
        if not isinstance(order, PublicOrderResult):
            if not isinstance(order, OrderResult):
                raise RuntimeError("trade service returned an invalid actionable order")
        view = OrderActionView(controller, order)
        if view.children:
            bot.add_view(view)
    return len(values)


def register_trade_ui(
    bot: Any,
    controller: DiscordTradeUI,
    *,
    accepting_orders_requested: bool | None = None,
) -> app_commands.Group:
    advertised_live = (
        controller.accepting_orders
        if accepting_orders_requested is None
        else bool(accepting_orders_requested)
    )
    pause_prefix = "" if advertised_live else "PAUSED: "
    group = app_commands.Group(
        name="trade",
        description=(
            "Buy and sell Bitcoin 09"
            if advertised_live
            else "PAUSED: new Bitcoin 09 OTC offers"
        ),
    )

    @group.command(name="sell", description=f"{pause_prefix}Create a WTS offer")
    @app_commands.autocomplete(asset=_asset_autocomplete)
    async def trade_sell(interaction: discord.Interaction, amount: str | None = None, total_price: str | None = None, asset: str | None = None, method: str | None = None, network: str | None = None, receive_address: str | None = None) -> None:
        if None in (amount, total_price, asset, method):
            await interaction.response.send_modal(TradeOrderModal(controller, side="sell"))
            return
        await controller.create_sell(interaction, amount, total_price, asset, method, network, receive_address)

    @group.command(name="buy", description=f"{pause_prefix}Create a WTB offer")
    @app_commands.autocomplete(asset=_asset_autocomplete)
    async def trade_buy(interaction: discord.Interaction, amount: str | None = None, total_price: str | None = None, asset: str | None = None, method: str | None = None, network: str | None = None, receive_address: str | None = None) -> None:
        if None in (amount, total_price, asset, method):
            await interaction.response.send_modal(TradeOrderModal(controller, side="buy"))
            return
        await controller.create_buy(interaction, amount, total_price, asset, method, network, receive_address)

    @group.command(
        name="list",
        description=(
            "List open WTS and WTB offers"
            if advertised_live
            else "List existing WTS and WTB offers (new offers paused)"
        ),
    )
    async def trade_list(interaction: discord.Interaction) -> None:
        await controller.list_orders(interaction)

    @group.command(name="view", description="View an order")
    async def trade_view(interaction: discord.Interaction, order_id: int) -> None:
        await controller.view_order(interaction, order_id)

    @group.command(name="accept", description=f"{pause_prefix}Accept an open order")
    async def trade_accept(interaction: discord.Interaction, order_id: int, receive_address: str | None = None) -> None:
        await controller.accept(interaction, order_id, receive_address)

    @group.command(name="deposit", description="Seller: check the assigned 09C deposit")
    async def trade_deposit(interaction: discord.Interaction, order_id: int) -> None:
        await controller.check_deposit(interaction, order_id)

    @group.command(name="confirm-sent", description="Buyer: confirm outside payment sent")
    async def trade_confirm_sent(interaction: discord.Interaction, order_id: int) -> None:
        await controller.confirm_sent(interaction, order_id)

    @group.command(name="confirm-received", description="Seller: confirm outside payment received")
    async def trade_confirm_received(interaction: discord.Interaction, order_id: int) -> None:
        await controller.confirm_received(interaction, order_id)

    @group.command(name="cancel", description="Cancel or leave an order when safely allowed")
    async def trade_cancel(interaction: discord.Interaction, order_id: int) -> None:
        await controller.cancel(interaction, order_id)

    @group.command(name="dispute", description="Participant: open a private dispute")
    async def trade_dispute(interaction: discord.Interaction, order_id: int, reason: str | None = None) -> None:
        if reason is None:
            await interaction.response.send_modal(DisputeModal(controller, order_id=order_id))
            return
        await controller.dispute(interaction, order_id, reason)

    @group.command(name="address", description="Save your validated 09C receive/refund address")
    async def trade_address(interaction: discord.Interaction, address: str | None = None) -> None:
        if address is None:
            await interaction.response.send_modal(AddressModal(controller))
            return
        await controller.set_address(interaction, address)

    @group.command(name="balance", description="Show your non-custodial OTC order status")
    async def trade_balance(interaction: discord.Interaction) -> None:
        await controller.balance(interaction)

    @group.command(name="resolve", description="Admin: resolve a disputed order")
    async def trade_resolve(interaction: discord.Interaction, order_id: int, winner: str, reason: str) -> None:
        await controller.resolve(interaction, order_id, winner, reason)

    @group.command(name="reconcile", description="Admin: run safe transfer recovery")
    async def trade_reconcile(interaction: discord.Interaction) -> None:
        await controller.reconcile(interaction)

    @group.command(name="mine", description="Admin: process one queued wallet operation")
    async def trade_mine(interaction: discord.Interaction) -> None:
        await controller.mine(interaction)

    @group.command(name="withdraw", description="Admin: reserve a confirmed platform-fee withdrawal")
    async def trade_withdraw(interaction: discord.Interaction, amount: str, destination: str) -> None:
        await controller.withdraw(interaction, amount, destination)

    bot.tree.add_command(group)
    bot.tree.add_command(
        app_commands.ContextMenu(
            name="Translate to English",
            callback=controller.translate_message,
        )
    )
    _register_legacy_wrappers(
        bot, controller, accepting_orders_requested=advertised_live
    )
    return group


def _register_legacy_wrappers(
    bot: Any,
    controller: DiscordTradeUI,
    *,
    accepting_orders_requested: bool,
) -> None:
    tree = bot.tree
    pause_prefix = "" if accepting_orders_requested else "PAUSED: "

    @tree.command(
        name="sell", description=f"{pause_prefix}Compatibility wrapper; use /trade sell"
    )
    @app_commands.autocomplete(asset=_asset_autocomplete)
    async def sell(interaction: discord.Interaction, amount: str, total_price: str, asset: str, method: str, network: str | None = None, receive_address: str | None = None) -> None:
        await controller.create_sell(interaction, amount, total_price, asset, method, network, receive_address)

    @tree.command(
        name="orders",
        description=(
            "Compatibility wrapper; use /trade list"
            if accepting_orders_requested
            else "Compatibility wrapper; use /trade list (new offers paused)"
        ),
    )
    async def orders(interaction: discord.Interaction) -> None:
        await controller.list_orders(interaction)

    @tree.command(
        name="buy", description=f"{pause_prefix}Compatibility wrapper; use /trade accept"
    )
    async def buy(interaction: discord.Interaction, order_id: int, receive_address: str | None = None) -> None:
        await controller.accept(interaction, order_id, receive_address)

    @tree.command(name="deposit", description="Compatibility wrapper; use /trade deposit")
    async def deposit(interaction: discord.Interaction, order_id: int) -> None:
        await controller.check_deposit(interaction, order_id)

    @tree.command(name="confirm", description="Compatibility wrapper; use /trade confirm-sent or confirm-received")
    async def confirm(interaction: discord.Interaction, order_id: int) -> None:
        await controller.legacy_confirm(interaction, order_id)

    @tree.command(name="cancel", description="Compatibility wrapper; use /trade cancel")
    async def cancel(interaction: discord.Interaction, order_id: int) -> None:
        await controller.cancel(interaction, order_id)

    @tree.command(name="dispute", description="Compatibility wrapper; use /trade dispute")
    async def dispute(interaction: discord.Interaction, order_id: int, reason: str) -> None:
        await controller.dispute(interaction, order_id, reason)

    @tree.command(name="setaddress", description="Compatibility wrapper; use /trade address")
    async def setaddress(interaction: discord.Interaction, address: str) -> None:
        await controller.set_address(interaction, address)

    @tree.command(name="balance", description="Compatibility wrapper; use /trade balance")
    async def balance(interaction: discord.Interaction) -> None:
        await controller.balance(interaction)

    @tree.command(name="withdraw", description="Compatibility wrapper; use /trade withdraw")
    async def withdraw(interaction: discord.Interaction, amount: str, destination: str) -> None:
        await controller.withdraw(interaction, amount, destination)
