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


if __name__ == "__main__":
    unittest.main()
