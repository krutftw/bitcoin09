from __future__ import annotations

import asyncio
import io
import re
import ast
import inspect
import tempfile
import threading
import unittest
import warnings
from contextlib import redirect_stdout
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import discord
from discord.ext import commands

from bot.otc.service import AccountStatus, AuthorizationError, OrderResult
from bot.otc.translation import TranslationBusy, TranslationUnavailable
from bot.otc import domain as otc_domain
from bot.btc09_otc_bot import Config, OTCBot, _probe_explorer_anchors, build_runtime
from bot.otc.explorer import AddressBatch, Tip, TransactionStatus
from bot.otc.discord_ui import (
    COMMON_ASSETS,
    MAINTENANCE_NOTICE,
    TRADE_HELP,
    DiscordTradeUI,
    AddressModal,
    DisputeModal,
    OrderActionView,
    TradeOrderModal,
    action_custom_id,
    asset_choices,
    render_public_order,
    register_trade_ui,
    register_persistent_order_views,
)


def order(**changes: object) -> OrderResult:
    base = OrderResult(
        order_id=42,
        side="sell",
        state="matched",
        maker_id=111111111111111111,
        buyer_id=222222222222222222,
        seller_id=111111111111111111,
        net_amount_units=125_000_000,
        network_fee_units=10_000,
        service_fee_units=0,
        deposit_required_units=125_010_000,
        total_price="250.50",
        settlement_asset="AUD",
        settlement_network="TRC20",
        payment_method="PayID",
        deposit_addr="5JThisMustNeverBePublicWalletAddress",
        deposit_deadline=123,
        buyer_confirmed=False,
        seller_confirmed=False,
        deposit_credited_units=125_010_000,
        deposit_main_units=125_010_000,
        deposit_recovery_units=0,
        accepted=True,
        events=(),
    )
    return replace(base, **changes)


class FakeResponse:
    def __init__(self) -> None:
        self.deferred: list[bool] = []
        self.sent: list[tuple[str, bool, object | None]] = []
        self.edited: list[tuple[str, object | None]] = []
        self._done = False
        self.edit_error: BaseException | None = None
        self.edit_delay = 0.0

    def is_done(self) -> bool:
        return self._done

    async def defer(self, *, ephemeral: bool = False, thinking: bool = True) -> None:
        if self._done:
            raise RuntimeError("interaction response already completed")
        self._done = True
        self.deferred.append(ephemeral)

    async def send_message(
        self, content: str, *, ephemeral: bool = False, view: object | None = None
    ) -> None:
        if self._done:
            raise RuntimeError("interaction response already completed")
        self._done = True
        self.sent.append((content, ephemeral, view))

    async def edit_message(self, *, content: str, view: object | None = None) -> None:
        if self._done:
            raise RuntimeError("interaction response already completed")
        if self.edit_delay:
            await asyncio.sleep(self.edit_delay)
        if self.edit_error is not None:
            raise self.edit_error
        self._done = True
        self.edited.append((content, view))


class FakeFollowup:
    def __init__(self) -> None:
        self.sent: list[tuple[str, bool, object | None]] = []

    async def send(
        self, content: str, *, ephemeral: bool = False, view: object | None = None
    ) -> None:
        self.sent.append((content, ephemeral, view))


class FakeUser:
    def __init__(self, user_id: int, display_name: str = "Trader") -> None:
        self.id = user_id
        self.display_name = display_name


class FakeInteraction:
    def __init__(self, interaction_id: int, user_id: int) -> None:
        self.id = interaction_id
        self.user = FakeUser(user_id)
        self.response = FakeResponse()
        self.followup = FakeFollowup()


class FakeMessage:
    def __init__(self, content: str) -> None:
        self.content = content


class FakeService:
    def __init__(self, result: OrderResult | None = None) -> None:
        self.result = result or order()
        self.calls: list[tuple[str, tuple[object, ...], dict[str, object]]] = []

    def _call(self, name: str, *args: object, **kwargs: object) -> OrderResult:
        self.calls.append((name, args, kwargs))
        return self.result

    def create_sell(self, **kwargs: object) -> OrderResult:
        return self._call("create_sell", **kwargs)

    def create_buy(self, **kwargs: object) -> OrderResult:
        return self._call("create_buy", **kwargs)

    def accept(self, *args: object, **kwargs: object) -> OrderResult:
        return self._call("accept", *args, **kwargs)

    def check_deposit(self, *args: object, **kwargs: object) -> OrderResult:
        if kwargs.get("actor_id") != self.result.seller_id:
            raise AuthorizationError("only the assigned seller may check this deposit")
        return self._call("check_deposit", *args, **kwargs)

    def confirm_sent(self, *args: object, **kwargs: object) -> OrderResult:
        if kwargs.get("actor_id") != self.result.buyer_id:
            raise AuthorizationError("only the assigned buyer may confirm payment sent")
        return self._call("confirm_sent", *args, **kwargs)

    def confirm_received(self, *args: object, **kwargs: object) -> OrderResult:
        if kwargs.get("actor_id") != self.result.seller_id:
            raise AuthorizationError("only the assigned seller may confirm payment received")
        return self._call("confirm_received", *args, **kwargs)

    def open_dispute(self, *args: object, **kwargs: object) -> OrderResult:
        if kwargs.get("actor_id") not in {self.result.buyer_id, self.result.seller_id}:
            raise AuthorizationError("only a trade participant may open a dispute")
        return self._call("open_dispute", *args, **kwargs)

    def resolve_dispute(self, *args: object, **kwargs: object) -> OrderResult:
        return self._call("resolve_dispute", *args, **kwargs)

    def cancel(self, *args: object, **kwargs: object) -> OrderResult:
        return self._call("cancel", *args, **kwargs)

    def list_open(self) -> tuple[OrderResult, ...]:
        self.calls.append(("list_open", (), {}))
        return (self.result,)

    def list_actionable_public(self) -> tuple[OrderResult, ...]:
        self.calls.append(("list_actionable_public", (), {}))
        return (self.result,)

    def reconcile_transfers(self) -> tuple[object, ...]:
        self.calls.append(("reconcile_transfers", (), {}))
        return ()

    def available_fee_units(self) -> int:
        self.calls.append(("available_fee_units", (), {}))
        return 0

    def reserve_fee_withdrawal(self, **kwargs: object) -> None:
        self.calls.append(("reserve_fee_withdrawal", (), kwargs))
        return None

    def mine(self) -> None:
        self.calls.append(("mine", (), {}))
        return None

    def account_status(self, **kwargs: object) -> AccountStatus:
        self.calls.append(("account_status", (), kwargs))
        return AccountStatus(2, 1, 1, 0)


class DiscordTradeUITests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self.service = FakeService()
        self.ui = DiscordTradeUI(
            self.service, admin_ids={9001}, accepting_orders=True, executor=self._run
        )

    @staticmethod
    async def _run(call):
        return call()

    async def test_translate_message_is_ephemeral_and_reports_unavailable(self) -> None:
        class UnavailableProvider:
            def translate_to_english(self, text: str) -> str:
                raise TranslationUnavailable("translation unavailable")

        ui = DiscordTradeUI(
            self.service,
            admin_ids={9001},
            accepting_orders=True,
            executor=self._run,
            translation_provider=UnavailableProvider(),
        )
        interaction = FakeInteraction(91, 1)
        await ui.translate_message(interaction, FakeMessage("non-English source"))
        self.assertEqual(interaction.response.deferred, [True])
        self.assertEqual(
            interaction.followup.sent,
            [("English translation is unavailable right now.", True, None)],
        )
        await ui.close_translation()

    async def test_translate_message_returns_only_ephemeral_english(self) -> None:
        class Provider:
            def translate_to_english(self, text: str) -> str:
                self.seen = text
                return "English result"

        provider = Provider()
        ui = DiscordTradeUI(
            self.service,
            admin_ids={9001},
            accepting_orders=True,
            executor=self._run,
            translation_provider=provider,
        )
        interaction = FakeInteraction(92, 1)
        await ui.translate_message(interaction, FakeMessage("source"))
        self.assertEqual(provider.seen, "source")
        self.assertEqual(interaction.followup.sent, [("English result", True, None)])
        await ui.close_translation()

    async def test_translate_flood_reports_busy_ephemerally(self) -> None:
        class BusyExecutor:
            async def translate_to_english(self, text: str) -> str:
                raise TranslationBusy("busy")

        ui = DiscordTradeUI(
            self.service,
            admin_ids={9001},
            accepting_orders=True,
            executor=self._run,
            translation_executor=BusyExecutor(),
        )
        interaction = FakeInteraction(93, 1)
        await ui.translate_message(interaction, FakeMessage("source"))
        self.assertEqual(
            interaction.followup.sent,
            [("English translation is busy. Please try again shortly.", True, None)],
        )

    def test_public_summary_is_complete_english_and_private(self) -> None:
        text = render_public_order(self.service.result)
        for expected in ("WTS", "1.25 09C", "250.50 AUD", "TRC20", "PayID", "matched"):
            self.assertIn(expected, text)
        for secret in (
            "111111111111111111",
            "222222222222222222",
            "5JThisMustNeverBePublicWalletAddress",
        ):
            self.assertNotIn(secret, text)
        self.assertIsNone(re.search(r"[\u3400-\u9fff]", text))

    def test_common_and_custom_asset_autocomplete_is_english_and_validated(self) -> None:
        self.assertEqual(
            COMMON_ASSETS,
            ("AUD", "USD", "EUR", "GBP", "CNY", "JPY", "USDT", "USDC", "BTC", "ETH", "SOL", "LTC", "DOGE", "BNB"),
        )
        self.assertIn("XAU", asset_choices("xau"))
        self.assertNotIn("BAD CODE", asset_choices("bad code"))
        self.assertTrue(all(re.fullmatch(r"[A-Z0-9._-]{2,12}", item) for item in asset_choices("")))

    async def test_trade_modal_explains_payment_method_and_network_fields(self) -> None:
        modal = TradeOrderModal(self.ui, side="sell")
        self.assertIn("Wise", modal.method.placeholder or "")
        self.assertIn("wallet transfer", (modal.method.placeholder or "").lower())
        self.assertIn("blank", modal.network.to_component_dict()["label"].lower())
        self.assertIn("TRC20", modal.network.placeholder or "")
        self.assertIn("bank", (modal.network.placeholder or "").lower())

    async def test_validation_rejection_is_not_logged_as_a_system_error(self) -> None:
        class RejectingService(FakeService):
            def create_sell(self, **kwargs: object) -> OrderResult:
                raise ValueError(
                    "USDT is the asset. Use TRC20, ERC20, or another supported "
                    "network; leave network blank for Wise or bank payments."
                )

        ui = DiscordTradeUI(
            RejectingService(),
            admin_ids={9001},
            accepting_orders=True,
            executor=self._run,
        )
        interaction = FakeInteraction(299, 20)
        output = io.StringIO()

        with redirect_stdout(output):
            await ui.create_sell(
                interaction,
                "39",
                "16",
                "USDT",
                "Wise",
                "USDT",
                None,
            )

        self.assertIn("[INFO] OTC interaction rejected", output.getvalue())
        self.assertNotIn("[ERROR] OTC interaction failed", output.getvalue())
        self.assertIn("USDT is the asset", interaction.followup.sent[-1][0])

    def test_custom_ids_contain_only_action_and_numeric_order(self) -> None:
        self.assertEqual(action_custom_id("confirm_sent", 42), "confirm_sent:42")
        with self.assertRaises(ValueError):
            action_custom_id("confirm_sent:222222222222222222", 42)

    async def test_state_views_are_persistent_and_coordinate_free(self) -> None:
        expected = {
            "open": {"accept:42", "cancel:42"},
            "awaiting_deposit": {"deposit:42", "cancel:42"},
            "matched": {
                "confirm_sent:42",
                "confirm_received:42",
                "cancel:42",
                "dispute:42",
            },
        }
        for state, custom_ids in expected.items():
            view = OrderActionView(self.ui, order(state=state))
            self.assertIsNone(view.timeout)
            self.assertTrue(view.is_persistent())
            self.assertEqual(
                {item.custom_id for item in view.children}, custom_ids
            )
        self.assertTrue(issubclass(TradeOrderModal, discord.ui.Modal))
        self.assertTrue(issubclass(DisputeModal, discord.ui.Modal))
        self.assertTrue(issubclass(AddressModal, discord.ui.Modal))

    def test_ui_and_composition_root_keep_custody_boundaries_out(self) -> None:
        root = Path(__file__).resolve().parents[1]
        ui_source = (root / "otc" / "discord_ui.py").read_text(encoding="utf-8")
        ui_imports = {
            node.names[0].name.split(".")[0]
            for node in ast.walk(ast.parse(ui_source))
            if isinstance(node, ast.Import)
        }
        ui_from_imports = {
            (node.module or "").split(".")[0]
            for node in ast.walk(ast.parse(ui_source))
            if isinstance(node, ast.ImportFrom)
        }
        self.assertTrue(
            {"sqlite3", "subprocess", "requests"}.isdisjoint(
                ui_imports | ui_from_imports
            )
        )
        composition = (root / "btc09_otc_bot.py").read_text(encoding="utf-8")
        self.assertNotIn("sqlite3", composition)
        self.assertNotIn("subprocess", composition)
        self.assertNotIn("requests", composition)
        self.assertNotIn("CREATE TABLE", composition)

    def test_startup_and_reconciliation_failures_invalidate_public_feed(self) -> None:
        root = Path(__file__).resolve().parents[1]
        composition = (root / "btc09_otc_bot.py").read_text(encoding="utf-8")
        self.assertGreaterEqual(composition.count("invalidate_public_feed"), 3)

    def test_close_orders_feed_and_translation_shutdown_before_invalidation(self) -> None:
        root = Path(__file__).resolve().parents[1]
        tree = ast.parse((root / "btc09_otc_bot.py").read_text(encoding="utf-8"))
        close = next(
            node
            for node in ast.walk(tree)
            if isinstance(node, ast.AsyncFunctionDef) and node.name == "close"
        )
        rendered = ast.unparse(close)
        self.assertIn("_feed_executor.shutdown", rendered)
        self.assertIn("await self.runtime.controller.close_translation()", rendered)
        self.assertLess(
            rendered.index("_feed_executor.shutdown"),
            rendered.index("invalidate_public_feed"),
        )

    def test_guild_sync_prunes_retired_global_commands_after_copy(self) -> None:
        source = inspect.getsource(OTCBot.setup_hook)
        self.assertIn("self.tree.clear_commands(guild=None)", source)
        guild_sync = source.index("await self.tree.sync(guild=guild)")
        prune = source.index("self.tree.clear_commands(guild=None)")
        global_sync = source.index("await self.tree.sync()", prune)
        self.assertLess(guild_sync, prune)
        self.assertLess(prune, global_sync)

    def test_public_settlement_allowlists_have_one_domain_owner(self) -> None:
        methods = getattr(otc_domain, "PUBLIC_SETTLEMENT_METHODS")
        networks = getattr(otc_domain, "PUBLIC_SETTLEMENT_NETWORKS")
        self.assertIn("PayID", methods)
        self.assertIn("TRC20", networks)
        root = Path(__file__).resolve().parents[1] / "otc"
        for module in ("discord_ui.py", "public_feed.py", "service.py"):
            source = (root / module).read_text(encoding="utf-8")
            self.assertNotIn("_PUBLIC_METHODS =", source)
            self.assertNotIn("_PUBLIC_NETWORKS =", source)

    async def test_close_waits_for_inflight_feed_then_invalidates_last(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "feed.json"
            runtime = SimpleNamespace(
                service=self.service,
                controller=self.ui,
                public_feed_path=str(target),
            )
            with patch.dict("os.environ", {"BOT_TOKEN": "test-token"}, clear=True):
                config = Config.from_environment()
            with warnings.catch_warnings():
                warnings.filterwarnings(
                    "ignore",
                    message=".*asyncio.iscoroutinefunction.*",
                    category=DeprecationWarning,
                )
                bot = OTCBot(config, runtime)
            started = threading.Event()
            release = threading.Event()

            def delayed_write() -> None:
                started.set()
                if not release.wait(5):
                    raise RuntimeError("test feed release timed out")
                target.write_text("late healthy feed", encoding="utf-8")

            pending = asyncio.create_task(bot._run_feed_job(delayed_write))
            self.assertTrue(await asyncio.to_thread(started.wait, 2))
            self.ui.accepting_orders = True
            observed_accepting: list[bool] = []
            observer = threading.Timer(
                0.01, lambda: observed_accepting.append(self.ui.accepting_orders)
            )
            timer = threading.Timer(0.02, release.set)
            observer.start()
            timer.start()
            try:
                await bot.close()
                await pending
            finally:
                observer.cancel()
                timer.cancel()
            self.assertEqual(observed_accepting, [False])
            self.assertFalse(target.exists())

    async def test_otc_bot_keeps_node_owned_commands_in_its_sync_surface(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            runtime = SimpleNamespace(
                service=self.service,
                controller=self.ui,
                public_feed_path=str(Path(directory) / "feed.json"),
            )
            with patch.dict("os.environ", {"BOT_TOKEN": "test-token"}, clear=True):
                config = Config.from_environment()
            with warnings.catch_warnings():
                warnings.filterwarnings(
                    "ignore",
                    message=".*asyncio.iscoroutinefunction.*",
                    category=DeprecationWarning,
                )
                bot = OTCBot(config, runtime)
            try:
                commands_by_name = {
                    command.name: command for command in bot.tree.get_commands()
                }
                self.assertTrue(
                    {"stats", "rank", "leaderboard"} <= commands_by_name.keys()
                )
                self.assertEqual(
                    {
                        name: commands_by_name[name].description
                        for name in ("stats", "rank", "leaderboard")
                    },
                    {
                        "stats": "Show live Bitcoin 09 mining and network stats.",
                        "rank": "Show your Bitcoin 09 community activity level.",
                        "leaderboard": "Show the Bitcoin 09 community activity leaderboard.",
                    },
                )
            finally:
                await bot.close()

    def test_explorer_anchor_probe_rejects_mismatched_transaction_tip(self) -> None:
        tip = Tip("a" * 64, 10)
        other = Tip("b" * 64, 10)

        class ExplorerProbe:
            network = "btc09-mainnet"

            @staticmethod
            def tip():
                return tip

            @staticmethod
            def batch_outputs(read_watched):
                self.assertEqual(tuple(read_watched()), ())
                return AddressBatch("btc09-mainnet", tip, ())

            @staticmethod
            def transaction(txid):
                return TransactionStatus(txid, "unknown", None, 0, other)

        evidence = _probe_explorer_anchors(ExplorerProbe(), lambda: ())
        self.assertFalse(evidence["explorer_tx_status_reachable"])
        self.assertIn("explorer_transaction_tip_mismatch", evidence["issues"])

    def test_explorer_anchor_probe_rejects_mismatched_empty_batch_tip(self) -> None:
        tip = Tip("a" * 64, 10)
        other = Tip("b" * 64, 10)

        class ExplorerProbe:
            network = "btc09-mainnet"

            @staticmethod
            def tip():
                return tip

            @staticmethod
            def batch_outputs(read_watched):
                self.assertEqual(tuple(read_watched()), ())
                return AddressBatch("btc09-mainnet", other, ())

            @staticmethod
            def transaction(txid):
                return TransactionStatus(txid, "unknown", None, 0, tip)

        evidence = _probe_explorer_anchors(ExplorerProbe(), lambda: ())
        self.assertFalse(evidence["explorer_snapshot_reachable"])
        self.assertIn("explorer_snapshot_tip_mismatch", evidence["issues"])

    async def test_seller_only_deposit_and_participant_only_actions(self) -> None:
        outsider = FakeInteraction(1, 333)
        await self.ui.check_deposit(outsider, 42)
        self.assertIn("only the assigned seller", outsider.followup.sent[-1][0])
        self.assertEqual(self.service.calls, [])

        await self.ui.confirm_sent(FakeInteraction(2, 333), 42, confirmed=True)
        await self.ui.dispute(FakeInteraction(3, 333), 42, "A detailed private reason")
        self.assertEqual(self.service.calls, [])

    async def test_error_followup_omits_absent_view_for_discord_webhook(self) -> None:
        class StrictFollowup(FakeFollowup):
            async def send(self, content: str, **kwargs: object) -> None:
                if "view" in kwargs and kwargs["view"] is None:
                    raise TypeError("expected view parameter to be of type View not NoneType")
                self.sent.append(
                    (
                        content,
                        bool(kwargs.get("ephemeral", False)),
                        kwargs.get("view"),
                    )
                )

        outsider = FakeInteraction(991140, 333)
        outsider.followup = StrictFollowup()
        await self.ui.check_deposit(outsider, 42)
        self.assertIn("only the assigned seller", outsider.followup.sent[-1][0])

    async def test_only_configured_admin_can_resolve(self) -> None:
        denied = FakeInteraction(4, 42)
        await self.ui.resolve(denied, 42, "buyer", "A sufficiently detailed reason", confirmed=True)
        self.assertEqual(self.service.calls, [])
        denied_message = (denied.followup.sent or denied.response.sent)[-1][0]
        self.assertIn("administrator", denied_message)

        allowed = FakeInteraction(5, 9001)
        await self.ui.resolve(allowed, 42, "buyer", "A sufficiently detailed reason", confirmed=True)
        self.assertEqual(self.service.calls[-1][0], "resolve_dispute")

    async def test_duplicate_interaction_returns_cached_current_state_once(self) -> None:
        interaction = FakeInteraction(77, self.service.result.buyer_id or 0)
        await self.ui.confirm_sent(interaction, 42, confirmed=True)
        await self.ui.confirm_sent(interaction, 42, confirmed=True)
        self.assertEqual([call[0] for call in self.service.calls], ["confirm_sent"])
        self.assertEqual(len(interaction.followup.sent), 2)
        self.assertEqual(interaction.followup.sent[0][0], interaction.followup.sent[1][0])
        self.assertEqual(interaction.response.deferred, [True])

    async def test_fund_moving_action_requires_ephemeral_confirmation(self) -> None:
        interaction = FakeInteraction(88, self.service.result.buyer_id or 0)
        await self.ui.confirm_sent(interaction, 42)
        self.assertEqual(self.service.calls, [])
        content, ephemeral, view = interaction.followup.sent[-1]
        self.assertTrue(ephemeral)
        self.assertIn("Confirm", content)
        self.assertIsNotNone(view)

    async def test_fee_withdrawal_keeps_initial_intent_operation_key(self) -> None:
        ui = DiscordTradeUI(
            self.service,
            admin_ids={9001},
            accepting_orders=True,
            admin_fee_destination="5ConfiguredDestination",
            executor=self._run,
        )
        initial = FakeInteraction(991122, 9001)
        await ui.withdraw(initial, "1", "5ConfiguredDestination")
        view = initial.followup.sent[-1][2]
        self.assertIsNotNone(view)
        click = FakeInteraction(991999, 9001)
        await view.children[0].callback(click)
        reservation = next(call for call in self.service.calls if call[0] == "reserve_fee_withdrawal")
        self.assertEqual(
            reservation[2]["operation_key"], "discord:991122:fee_withdrawal"
        )
        self.assertNotIn("mine", [call[0] for call in self.service.calls])

    async def test_balance_is_noncustodial_and_private(self) -> None:
        interaction = FakeInteraction(991123, 20)
        await self.ui.balance(interaction)
        content, ephemeral, _ = interaction.followup.sent[-1]
        self.assertTrue(ephemeral)
        self.assertIn("does not hold a custodial user account balance", content)
        self.assertNotIn("liability", content.lower())

    async def test_legacy_confirm_waits_for_ephemeral_confirmation(self) -> None:
        interaction = FakeInteraction(991124, self.service.result.buyer_id or 0)
        await self.ui.legacy_confirm(interaction, 42)
        self.assertEqual(self.service.calls, [])
        content, ephemeral, view = interaction.followup.sent[-1]
        self.assertTrue(ephemeral)
        self.assertIn("Confirm", content)
        self.assertIsNotNone(view)

    async def test_concurrent_distinct_confirmation_clicks_call_service_once(self) -> None:
        async def delayed(call):
            await asyncio.sleep(0.03)
            return call()

        ui = DiscordTradeUI(
            self.service,
            admin_ids={9001},
            accepting_orders=True,
            executor=delayed,
        )
        initial = FakeInteraction(991125, self.service.result.buyer_id or 0)
        await ui.confirm_sent(initial, 42)
        view = initial.followup.sent[-1][2]
        first = FakeInteraction(991126, self.service.result.buyer_id or 0)
        second = FakeInteraction(991127, self.service.result.buyer_id or 0)
        await asyncio.gather(
            view.children[0].callback(first),
            view.children[0].callback(second),
        )
        self.assertEqual([call[0] for call in self.service.calls], ["confirm_sent"])
        self.assertTrue(first.response.edited or second.response.edited)
        self.assertTrue(all(item.disabled for item in view.children))
        loser = second if first.response.edited else first
        self.assertEqual(loser.response.deferred, [True])
        loser_message = (loser.response.sent or loser.followup.sent)[-1][0]
        self.assertIn("Order #42", loser_message)

    async def test_edit_failure_settles_all_confirmation_clicks_without_service(self) -> None:
        initial = FakeInteraction(991130, self.service.result.buyer_id or 0)
        await self.ui.confirm_sent(initial, 42)
        view = initial.followup.sent[-1][2]
        owner = FakeInteraction(991131, self.service.result.buyer_id or 0)
        waiter = FakeInteraction(991132, self.service.result.buyer_id or 0)
        owner.response.edit_delay = 0.03
        owner.response.edit_error = RuntimeError("simulated Discord edit failure")
        results = await asyncio.wait_for(
            asyncio.gather(
                view.children[0].callback(owner),
                view.children[0].callback(waiter),
                return_exceptions=True,
            ),
            timeout=1,
        )
        self.assertIsInstance(results[0], RuntimeError)
        self.assertIsNone(results[1])
        self.assertEqual(self.service.calls, [])
        self.assertTrue(view._completion.done())
        self.assertTrue(all(item.disabled for item in view.children))
        self.assertEqual(waiter.response.deferred, [True])
        self.assertIn("No trade action was performed", waiter.followup.sent[-1][0])

    async def test_edit_cancellation_settles_waiter_and_reraises_to_owner(self) -> None:
        initial = FakeInteraction(991133, self.service.result.buyer_id or 0)
        await self.ui.confirm_sent(initial, 42)
        view = initial.followup.sent[-1][2]
        owner = FakeInteraction(991134, self.service.result.buyer_id or 0)
        waiter = FakeInteraction(991135, self.service.result.buyer_id or 0)
        owner.response.edit_delay = 0.03
        owner.response.edit_error = asyncio.CancelledError()
        results = await asyncio.wait_for(
            asyncio.gather(
                view.children[0].callback(owner),
                view.children[0].callback(waiter),
                return_exceptions=True,
            ),
            timeout=1,
        )
        self.assertIsInstance(results[0], asyncio.CancelledError)
        self.assertIsNone(results[1])
        self.assertEqual(self.service.calls, [])
        self.assertTrue(view._completion.done())
        self.assertEqual(waiter.response.deferred, [True])
        self.assertIn("No trade action was performed", waiter.followup.sent[-1][0])

    async def test_maintenance_blocks_only_create_and_accept(self) -> None:
        ui = DiscordTradeUI(
            self.service, admin_ids={9001}, accepting_orders=False, executor=self._run
        )
        for index, invoke in enumerate(
            (
                lambda i: ui.create_sell(i, "1", "2", "AUD", "PayID", None, None),
                lambda i: ui.create_buy(i, "1", "2", "AUD", "PayID", None, None),
                lambda i: ui.accept(i, 42),
            ),
            start=100,
        ):
            interaction = FakeInteraction(index, 20)
            await invoke(interaction)
            self.assertEqual(interaction.response.sent[-1][0], MAINTENANCE_NOTICE)
        self.assertEqual(self.service.calls, [])

        status = FakeInteraction(104, 20)
        await ui.list_orders(status)
        dispute = FakeInteraction(105, self.service.result.buyer_id or 0)
        await ui.dispute(dispute, 42, "A detailed private reason")
        recovery = FakeInteraction(106, 9001)
        await ui.reconcile(recovery, confirmed=True)
        self.assertEqual(
            [call[0] for call in self.service.calls],
            ["list_open", "open_dispute", "reconcile_transfers"],
        )

    async def test_service_work_is_deferred_then_followed_up(self) -> None:
        interaction = FakeInteraction(200, 20)
        await self.ui.list_orders(interaction)
        self.assertEqual(interaction.response.deferred, [False])
        self.assertEqual(len(interaction.followup.sent), 1)
        self.assertEqual(interaction.response.sent, [])

    async def test_registration_has_grouped_and_compatibility_commands_in_english(self) -> None:
        with warnings.catch_warnings():
            warnings.filterwarnings(
                "ignore",
                message=".*asyncio.iscoroutinefunction.*",
                category=DeprecationWarning,
            )
            bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
            try:
                group = register_trade_ui(bot, self.ui)
                self.assertEqual(group.name, "trade")
                self.assertTrue(
                    {
                        "sell", "buy", "list", "view", "accept", "deposit",
                        "confirm-sent", "confirm-received", "cancel", "dispute",
                        "address", "balance", "help", "resolve", "reconcile", "mine", "withdraw",
                    }
                    <= {command.name for command in group.commands}
                )
                self.assertTrue(
                    {"help", "sell", "orders", "buy", "deposit", "confirm", "cancel", "dispute", "setaddress", "balance", "withdraw", "Translate to English"}
                    <= {command.name for command in bot.tree.get_commands()}
                )
                rendered = " ".join(
                    f"{command.name} {command.description}"
                    for command in bot.tree.walk_commands()
                )
                self.assertIsNone(re.search(r"[\u3400-\u9fff]", rendered))
            finally:
                await bot.close()

    async def test_help_replies_immediately_with_current_trade_steps(self) -> None:
        interaction = FakeInteraction(201, 20)
        await self.ui.show_help(interaction)
        self.assertEqual(interaction.response.sent, [(TRADE_HELP, True, None)])
        self.assertIn("/trade list", TRADE_HELP)
        self.assertIn("/trade accept <order_id>", TRADE_HELP)

    async def test_disabled_registration_marks_only_blocked_new_order_commands_paused(
        self,
    ) -> None:
        ui = DiscordTradeUI(
            self.service, admin_ids={9001}, accepting_orders=False, executor=self._run
        )
        with warnings.catch_warnings():
            warnings.filterwarnings(
                "ignore",
                message=".*asyncio.iscoroutinefunction.*",
                category=DeprecationWarning,
            )
            bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
            try:
                group = register_trade_ui(bot, ui)
                descriptions = {
                    command.name: command.description for command in group.commands
                }
                self.assertTrue(group.description.startswith("PAUSED:"))
                for name in ("sell", "buy", "accept"):
                    self.assertTrue(descriptions[name].startswith("PAUSED:"))
                self.assertIn("new offers paused", descriptions["list"].lower())
                for name in ("deposit", "confirm-sent", "confirm-received", "dispute"):
                    self.assertFalse(descriptions[name].startswith("PAUSED:"))

                for name in ("sell", "buy"):
                    command = bot.tree.get_command(name)
                    self.assertIsNotNone(command)
                    self.assertTrue(command.description.startswith("PAUSED:"))
                orders = bot.tree.get_command("orders")
                self.assertIsNotNone(orders)
                self.assertIn("new offers paused", orders.description.lower())
                deposit = bot.tree.get_command("deposit")
                self.assertIsNotNone(deposit)
                self.assertFalse(deposit.description.startswith("PAUSED:"))
            finally:
                await bot.close()

    async def test_requested_live_registration_does_not_advertise_runtime_pause(
        self,
    ) -> None:
        ui = DiscordTradeUI(
            self.service, admin_ids={9001}, accepting_orders=False, executor=self._run
        )
        with warnings.catch_warnings():
            warnings.filterwarnings(
                "ignore",
                message=".*asyncio.iscoroutinefunction.*",
                category=DeprecationWarning,
            )
            bot = commands.Bot(command_prefix="!", intents=discord.Intents.none())
            try:
                group = register_trade_ui(
                    bot, ui, accepting_orders_requested=True
                )
                descriptions = {
                    command.name: command.description for command in group.commands
                }
                self.assertFalse(group.description.startswith("PAUSED:"))
                for name in ("sell", "buy", "accept"):
                    self.assertFalse(descriptions[name].startswith("PAUSED:"))
                self.assertNotIn("paused", descriptions["list"].lower())
                self.assertFalse(ui.accepting_orders)
            finally:
                await bot.close()

    def test_live_trade_copy_distinguishes_wtb_creation_from_order_acceptance(
        self,
    ) -> None:
        root = Path(__file__).resolve().parents[2]
        readme = (root / "README.md").read_text(encoding="utf-8")
        home = (root / "docs" / "index.html").read_text(encoding="utf-8")
        markets = (root / "docs" / "markets.html").read_text(encoding="utf-8")

        self.assertIn("`/trade buy` to create a WTB offer", readme)
        self.assertIn("`/trade accept <order_id>` to accept", readme)
        for page in (home, markets):
            self.assertIn("<code>/trade buy</code> to create a WTB offer", page)
            self.assertIn("<code>/trade accept &lt;order_id&gt;</code> to accept", page)

    async def test_restart_registers_persistent_views_for_durable_orders(self) -> None:
        class ViewBot:
            def __init__(self) -> None:
                self.views: list[object] = []

            def add_view(self, view: object) -> None:
                self.views.append(view)

        bot = ViewBot()
        count = await register_persistent_order_views(bot, self.ui)
        self.assertEqual(count, 1)
        self.assertEqual(len(bot.views), 1)
        self.assertIsNone(bot.views[0].timeout)
        self.assertEqual(self.service.calls, [("list_actionable_public", (), {})])

    def test_config_repr_never_contains_discord_token(self) -> None:
        sentinel = "discord-token-SENTINEL-never-log"
        with patch.dict("os.environ", {"BOT_TOKEN": sentinel}, clear=True):
            config = Config.from_environment()
        rendered = repr(config)
        self.assertNotIn(sentinel, rendered)
        self.assertNotIn("token=", rendered)

    def test_active_order_capacity_config_defaults_validation_and_propagation(self) -> None:
        with patch.dict("os.environ", {"BOT_TOKEN": "test-token"}, clear=True):
            config = Config.from_environment()
        self.assertEqual(config.max_active_orders_total, 500)
        self.assertEqual(config.max_active_orders_per_maker, 20)
        self.assertEqual(config.max_deposit_allocations_lifetime_total, 5_000)
        self.assertEqual(config.max_deposit_allocations_lifetime_per_seller, 250)
        self.assertEqual(config.max_deposit_allocations_daily_total, 100)
        self.assertEqual(config.max_deposit_allocations_daily_per_seller, 10)
        source = ast.unparse(ast.parse(inspect.getsource(build_runtime)))
        self.assertIn("max_active_orders_total=config.max_active_orders_total", source)
        self.assertIn(
            "max_active_orders_per_maker=config.max_active_orders_per_maker", source
        )

        invalid_environments = (
            {"OTC_MAX_ACTIVE_ORDERS_TOTAL": "0"},
            {"OTC_MAX_ACTIVE_ORDERS_TOTAL": "1001"},
            {"OTC_MAX_ACTIVE_ORDERS_PER_MAKER": "0"},
            {
                "OTC_MAX_ACTIVE_ORDERS_TOTAL": "10",
                "OTC_MAX_ACTIVE_ORDERS_PER_MAKER": "11",
            },
            {"OTC_MAX_ACTIVE_ORDERS_TOTAL": "not-an-integer"},
            {"OTC_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL": "9001"},
            {
                "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL": "100",
                "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER": "101",
            },
            {
                "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_TOTAL": "100",
                "OTC_DEPOSIT_ALLOCATIONS_DAILY_TOTAL": "101",
            },
            {
                "OTC_DEPOSIT_ALLOCATIONS_LIFETIME_PER_SELLER": "5",
                "OTC_DEPOSIT_ALLOCATIONS_DAILY_PER_SELLER": "6",
            },
        )
        for values in invalid_environments:
            with self.subTest(values=values), patch.dict(
                "os.environ", {"BOT_TOKEN": "test-token", **values}, clear=True
            ), self.assertRaises(ValueError):
                Config.from_environment()


if __name__ == "__main__":
    unittest.main()
