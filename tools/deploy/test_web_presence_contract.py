import pathlib
import unittest


class WebPresenceContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.config = pathlib.Path("deploy/nginx/bitcoin09-site.conf").read_text(
            encoding="utf-8"
        )
        cls.nine_inbox = pathlib.Path(
            "deploy/nginx/bitcoin09-nine-inbox-server.conf"
        ).read_text(encoding="utf-8")

    def test_www_redirects_to_one_canonical_https_origin(self):
        self.assertIn("server_name www.btc09.org;", self.config)
        self.assertIn("return 301 https://btc09.org$request_uri;", self.config)
        self.assertIn("server_name btc09.org;", self.config)
        self.assertNotIn("server_name btc09.org www.btc09.org;", self.config)

    def test_root_and_explorer_share_baseline_security_headers(self):
        for token in (
            "X-Content-Type-Options nosniff",
            'X-Frame-Options "DENY"',
            'Referrer-Policy "strict-origin-when-cross-origin"',
            'Permissions-Policy "camera=(), microphone=(), geolocation=(), payment=()"',
            "server_name explorer.btc09.org;",
            "proxy_pass http://127.0.0.1:8009;",
        ):
            with self.subTest(token=token):
                self.assertIn(token, self.config)

    def test_proxy_hides_upstream_copies_of_headers_set_by_nginx(self):
        for name in (
            "X-Content-Type-Options",
            "X-Frame-Options",
            "Referrer-Policy",
            "Permissions-Policy",
        ):
            with self.subTest(name=name):
                self.assertEqual(
                    self.config.count(f"proxy_hide_header {name};"),
                    2,
                    "root and explorer TLS servers should each normalize this header",
                )

    def test_nine_inbox_proxy_hides_upstream_copies_of_security_headers(self):
        for name in (
            "X-Content-Type-Options",
            "X-Frame-Options",
            "Referrer-Policy",
            "Permissions-Policy",
        ):
            with self.subTest(name=name):
                self.assertEqual(
                    self.nine_inbox.count(f"proxy_hide_header {name};"),
                    6,
                    "each browser-facing Nine Inbox route should normalize this header",
                )


if __name__ == "__main__":
    unittest.main()
