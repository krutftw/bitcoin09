from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
HTTP = ROOT / "deploy" / "nginx" / "bitcoin09-nine-inbox-http.conf"
SERVER = ROOT / "deploy" / "nginx" / "bitcoin09-nine-inbox-server.conf"
SERVICE = ROOT / "deploy" / "systemd" / "btc09-nine-inbox.service"
INSTALLER = ROOT / "deploy" / "scripts" / "install-nine-inbox.sh"


class NineInboxDeploymentContract(unittest.TestCase):
    def test_http_limits_use_the_restored_client_address(self) -> None:
        text = HTTP.read_text(encoding="ascii")
        for zone in ("create", "read", "write", "connections"):
            self.assertIn(f"btc09_nine_{zone}", text)
        self.assertGreaterEqual(text.count("$binary_remote_addr"), 4)
        self.assertNotIn("$http_cf_connecting_ip", text)

    def test_only_inbox_and_versioned_api_reach_loopback(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        for route in (
            "location = /inbox",
            "location = /inbox/",
            "location = /inbox/share",
            "location ^~ /inbox/",
            "location = /api/nine/v1/inboxes",
            'location ~ "^/api/nine/v1/inboxes/',
        ):
            self.assertIn(route, text)
        self.assertIn("proxy_pass http://127.0.0.1:8020", text)
        self.assertNotIn("0.0.0.0:8020", text)
        self.assertNotIn("location /api/nine/", text)
        self.assertNotIn("server_name", text)

    def test_proxy_bounds_creation_uploads_connections_and_timeouts(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        for required in (
            "client_max_body_size 4k;",
            "client_max_body_size 21m;",
            "limit_req zone=btc09_nine_create",
            "limit_req zone=btc09_nine_read",
            "limit_req zone=btc09_nine_write",
            "limit_req_status 429;",
            "limit_conn btc09_nine_connections 8;",
            "proxy_request_buffering on;",
            "proxy_connect_timeout 2s;",
            "proxy_send_timeout 65s;",
            "proxy_read_timeout 65s;",
            "proxy_set_header X-Real-IP $remote_addr;",
            "Cache-Control \"no-cache\"",
            "Cache-Control \"public, max-age=3600\"",
        ):
            self.assertIn(required, text)

    def test_service_is_unprivileged_loopback_only_and_hardened(self) -> None:
        text = SERVICE.read_text(encoding="ascii")
        for required in (
            "User=btc09-nine-inbox",
            "Group=btc09-nine-inbox",
            "nine-inbox -listen 127.0.0.1:8020 -data-dir /var/lib/btc09-nine-inbox",
            "StateDirectory=btc09-nine-inbox",
            "NoNewPrivileges=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "PrivateTmp=true",
            "PrivateDevices=true",
            "IPAddressDeny=any",
            "IPAddressAllow=localhost",
            "MemoryMax=512M",
            "UMask=0077",
        ):
            self.assertIn(required, text)

    def test_installer_is_idempotent_and_checks_service_and_public_route(self) -> None:
        text = INSTALLER.read_text(encoding="ascii")
        for required in (
            "set -Eeuo pipefail",
            "go build -trimpath",
            "BTC09_NINE_BINARY_SOURCE",
            "BTC09_NINE_BINARY_SHA256",
            "sha256sum --check --strict",
            '[[ -f "$binary_source" && ! -L "$binary_source" ]]',
            "systemd-analyze verify",
            "nginx -t",
            "systemctl enable --now btc09-nine-inbox",
            "curl --fail --silent --show-error http://127.0.0.1:8020/healthz",
            "--resolve btc09.org:443:127.0.0.1",
            "https://btc09.org/inbox/",
            'website_source="$repo_root/docs/index.html"',
            "website_target=/var/www/bitcoin09/index.html",
            "restore_file \"$website_target\" website",
            "grep -Fq 'href=\"/inbox/\"'",
            "restore_install",
            "trap restore_install ERR INT TERM",
        ):
            self.assertIn(required, text)


if __name__ == "__main__":
    unittest.main()
