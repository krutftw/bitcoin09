from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
HTTP = ROOT / "deploy" / "nginx" / "bitcoin09-support-funding-http.conf"
SERVER = ROOT / "deploy" / "nginx" / "bitcoin09-support-funding-server.conf"
SERVICE = ROOT / "deploy" / "systemd" / "btc09-support-funding.service"
INSTALLER = ROOT / "deploy" / "scripts" / "install-support-funding.sh"


class SupportFundingDeploymentContract(unittest.TestCase):
    def test_http_limits_use_the_restored_client_address(self) -> None:
        text = HTTP.read_text(encoding="ascii")
        for zone in ("read", "create", "connections"):
            self.assertIn(f"btc09_support_{zone}", text)
        self.assertGreaterEqual(text.count("$binary_remote_addr"), 3)
        self.assertNotIn("$http_cf_connecting_ip", text)

    def test_only_exact_support_routes_reach_loopback(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        for route in (
            "location = /api/support/v1/status",
            "location = /api/support/v1/currencies",
            "location = /api/support/v1/payments",
            'location ~ "^/api/support/v1/payments/',
        ):
            self.assertIn(route, text)
        self.assertGreaterEqual(text.count("proxy_pass http://127.0.0.1:8032"), 4)
        self.assertNotIn("0.0.0.0:8032", text)
        self.assertNotIn("location /api/support/", text)
        self.assertNotIn("server_name", text)

    def test_proxy_bounds_creation_reads_connections_and_timeouts(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        for required in (
            "client_max_body_size 4k;",
            "limit_req zone=btc09_support_create",
            "limit_req zone=btc09_support_read",
            "limit_req_status 429;",
            "limit_conn btc09_support_connections",
            "proxy_request_buffering on;",
            "proxy_connect_timeout 2s;",
            "proxy_read_timeout 15s;",
            'Cache-Control "no-store"',
            "proxy_set_header X-Real-IP $remote_addr;",
        ):
            self.assertIn(required, text)

    def test_service_is_unprivileged_loopback_only_and_hardened(self) -> None:
        text = SERVICE.read_text(encoding="ascii")
        for required in (
            "User=btc09-support",
            "Group=btc09-support",
            "EnvironmentFile=/etc/btc09/support-funding.env",
            "funding-service.mjs",
            "StateDirectory=btc09-support",
            "ReadWritePaths=/var/lib/btc09-support",
            "NoNewPrivileges=true",
            "ProtectSystem=strict",
            "ProtectHome=true",
            "PrivateTmp=true",
            "PrivateDevices=true",
            "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
            "MemoryMax=256M",
            "UMask=0077",
        ):
            self.assertIn(required, text)

    def test_installer_checks_secret_permissions_and_live_routes(self) -> None:
        text = INSTALLER.read_text(encoding="ascii")
        for required in (
            "set -Eeuo pipefail",
            "support-funding.env",
            "must have mode 0600",
            "NOWPAYMENTS_API_KEY is missing",
            "node --check",
            "systemd-analyze verify",
            "nginx -t",
            "systemctl enable --now btc09-support-funding",
            "curl --fail --silent --show-error http://127.0.0.1:8032/healthz",
            "--resolve btc09.org:443:127.0.0.1",
            "https://btc09.org/api/support/v1/status",
            "restore_install",
            "trap restore_install ERR INT TERM",
        ):
            self.assertIn(required, text)


if __name__ == "__main__":
    unittest.main()
