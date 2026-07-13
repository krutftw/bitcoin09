import pathlib
import unittest


class IndexContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.html = pathlib.Path("docs/index.html").read_text(encoding="utf-8")

    def test_official_mining_metrics_are_rendered(self):
        for token in (
            "stat-network-hashrate",
            "stat-top-share-100",
            "stat-distinct-100",
            "estimated_network_hashrate_hps",
            "payout_address_windows",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_solo_calculator_is_explicit_about_variance(self):
        for token in (
            "solo-hashrate",
            "solo-estimate",
            "Expected time, not a guarantee",
            "function soloEstimate",
            "chanceInOneHour",
            "chanceInOneDay",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_official_hashrate_does_not_fall_back_to_pool_hashrate(self):
        self.assertNotIn(
            "estimated_network_hashrate_hps ?? pool",
            self.html,
        )
        self.assertIn("third-party, not run by 09C", self.html)

    def test_open_remote_solo_path_is_explicit_about_limits(self):
        for token in (
            "Open-source remote solo mining",
            "mine-pool",
            "-solo-api",
            "does not smooth solo-mining variance",
            "PPLNS is not live",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_v025_makes_the_official_wallet_miner_the_easy_path(self):
        for token in (
            "Current release: v0.1.25",
            "Desktop wallet and miner",
            "Open the wallet, choose Mine",
            "https://btc09.org/api/v1/work",
            "No partial-share payouts",
            "third-party closed-source binary miner",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_nine_inbox_is_a_plain_optional_utility(self):
        for token in (
            'href="/inbox/"',
            "Send yourself anything",
            "No account and no 09C needed",
            "client-side encrypted",
            "20 MiB",
            "seven days",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)
        self.assertNotIn("always delivered in the background", self.html.lower())

    def test_nine_inbox_documentation_covers_the_security_boundary(self):
        guide = pathlib.Path("docs/NINE-INBOX.md").read_text(encoding="utf-8")
        for token in (
            "https://btc09.org/inbox/",
            "does not need an account",
            "does not need 09C",
            "AES-256-GCM",
            "pairing link",
            "encrypted recovery file",
            "20 MiB",
            "seven days",
            "30 days",
            "server can see",
            "Background delivery",
        ):
            with self.subTest(token=token):
                self.assertIn(token, guide)


if __name__ == "__main__":
    unittest.main()
