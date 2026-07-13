import pathlib
import unittest


class IndexContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.html = pathlib.Path("docs/index.html").read_text(encoding="utf-8")
        cls.markets = pathlib.Path("docs/markets.html").read_text(encoding="utf-8")

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
        self.assertNotIn("ntmminer.com", self.html.lower())

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

    def test_v026_makes_the_supported_wallet_miner_the_easy_path(self):
        for token in (
            "Current release: v0.1.26",
            "Desktop wallet and miner",
            "Open the wallet, choose Mine",
            'id="mining-guide"',
            "Copy help report",
            "leaves out your wallet address",
            "https://btc09.org/api/v1/work",
            "No partial-share payouts",
            "Only download the official BTC09 client from GitHub",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_homepage_gives_newcomers_a_clear_first_path(self):
        for token in (
            'class="skip-link"',
            'aria-label="Primary navigation"',
            'href="#download"',
            'href="#network"',
            'href="#mining-guide"',
            'id="download"',
            "Download for Windows",
            "Create or open a wallet",
            "Receive, send, or mine",
            "Private keys stay on your computer",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_current_release_has_direct_platform_downloads_and_checksums(self):
        for asset in (
            "btc09-windows-amd64.exe",
            "btc09-macos-apple",
            "btc09-macos-intel",
            "btc09-linux-amd64",
            "btc09-linux-arm64",
            "SHA256SUMS",
        ):
            with self.subTest(asset=asset):
                self.assertIn(
                    f"https://github.com/krutftw/bitcoin09/releases/download/v0.1.26/{asset}",
                    self.html,
                )

    def test_long_operator_material_is_progressively_disclosed(self):
        self.assertGreaterEqual(self.html.count('class="technical-note"'), 4)
        for heading in (
            "Remote solo protocol",
            "Full node and command line",
            "Unofficial miner warning",
        ):
            with self.subTest(heading=heading):
                self.assertIn(f"<summary>{heading}</summary>", self.html)

    def test_public_mining_guidance_does_not_promote_retired_binary_links(self):
        surfaces = {
            "homepage": self.html,
            "readme": pathlib.Path("README.md").read_text(encoding="utf-8"),
            "discord setup": pathlib.Path("tools/discord/setup-server.mjs").read_text(
                encoding="utf-8"
            ),
            "discord stats": pathlib.Path("tools/discord/stats-bot.mjs").read_text(
                encoding="utf-8"
            ),
        }
        for surface_name, surface in surfaces.items():
            for forbidden in ("ntmminer", "mediafire", "bitcoin09.tutuit.xyz"):
                with self.subTest(surface=surface_name, forbidden=forbidden):
                    self.assertNotIn(forbidden, surface.lower())

        for token in (
            "Only download the official BTC09 client from GitHub",
            "There is no official 09C GPU miner",
            "Pooled payouts are not live in the official software",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_interactions_keep_visible_keyboard_and_reduced_motion_states(self):
        for token in (
            ":focus-visible",
            "prefers-reduced-motion: reduce",
            "scroll-margin-top",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)

    def test_live_network_summary_is_part_of_the_network_section(self):
        self.assertIn(
            '<p id="live" class="network-summary">',
            self.html,
        )
        self.assertNotIn('class="livebar"', self.html)

    def test_primary_hero_button_overrides_dark_section_link_color(self):
        self.assertIn(".hero .button {", self.html)
        self.assertIn("color: var(--coal);", self.html)

    def test_display_type_stays_compact(self):
        for token in (
            "font-size: clamp(42px, 5vw, 60px);",
            "font-size: clamp(28px, 3vw, 38px);",
            "font-size: 16px;",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)
        self.assertNotIn("font-size: clamp(48px, 6vw, 76px);", self.html)

    def test_explanatory_context_is_hidden_until_requested(self):
        for heading in (
            "Inbox limits and privacy",
            "Permissionless mining policy",
            "Miner help and privacy",
            "Trade boundaries",
        ):
            with self.subTest(heading=heading):
                self.assertIn(f"<summary>{heading}</summary>", self.html)

    def test_live_network_note_is_a_short_status_line(self):
        self.assertIn("note.textContent = 'Official node data. Updated '", self.html)

    def test_homepage_avoids_generic_ai_landing_page_structure(self):
        for token in (
            'class="download-list"',
            'class="download-row featured"',
            'class="project-facts"',
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.html)
        self.assertNotIn('class="download-grid"', self.html)
        self.assertNotIn('class="trust-strip"', self.html)

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

    def test_public_policy_pages_cover_the_real_service_boundaries(self):
        privacy = pathlib.Path("docs/privacy.html").read_text(encoding="utf-8")
        terms = pathlib.Path("docs/terms.html").read_text(encoding="utf-8")
        for token in (
            "Last updated: 13 July 2026",
            "No advertising or analytics",
            "Cloudflare",
            "DigitalOcean",
            "Nine Inbox",
            "Discord OTC bot",
            "Public blockchain data",
            "GitHub private security advisory",
        ):
            with self.subTest(page="privacy", token=token):
                self.assertIn(token, privacy)
        for token in (
            "Last updated: 13 July 2026",
            "self-custody",
            "irreversible",
            "Mining is probabilistic",
            "escrows only 09C",
            "outside the escrow",
            "not financial, legal, or tax advice",
            "third-party services",
        ):
            with self.subTest(page="terms", token=token):
                self.assertIn(token, terms)

    def test_policy_links_are_visible_on_public_entry_pages(self):
        for page_name, page in (("home", self.html), ("markets", self.markets)):
            for href in ('href="privacy.html"', 'href="terms.html"'):
                with self.subTest(page=page_name, href=href):
                    self.assertIn(href, page)

    def test_public_discovery_and_security_files_are_present(self):
        robots = pathlib.Path("docs/robots.txt").read_text(encoding="utf-8")
        sitemap = pathlib.Path("docs/sitemap.xml").read_text(encoding="utf-8")
        security = pathlib.Path("docs/.well-known/security.txt").read_text(encoding="utf-8")
        self.assertIn("Sitemap: https://btc09.org/sitemap.xml", robots)
        for url in (
            "https://btc09.org/",
            "https://btc09.org/markets.html",
            "https://btc09.org/inbox/",
            "https://btc09.org/privacy.html",
            "https://btc09.org/terms.html",
        ):
            with self.subTest(url=url):
                self.assertIn(f"<loc>{url}</loc>", sitemap)
        self.assertIn(
            "Contact: https://github.com/krutftw/bitcoin09/security/advisories/new",
            security,
        )


if __name__ == "__main__":
    unittest.main()
